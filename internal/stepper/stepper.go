// Package stepper runs one host's ranks for a step (spec sec. 7.5). It dials
// srun's control channel, receives the StepSpec, spawns a rank-exec trampoline
// per rank, frames the ranks' output back over the channel, forwards signals,
// and terminates its ranks if srun dies (the control channel drops).
package stepper

import (
	"bufio"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// killWait is how long the stepper waits after SIGTERM before SIGKILL when srun
// dies (a local default; the configurable kill_wait is applied srun-side).
const killWait = 5 * time.Second

// Run is the stepper entry point: `slurm-shim stepper --envelope <base64>`.
func Run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("stepper", flag.ContinueOnError)
	fs.SetOutput(stderr)
	envArg := fs.String("envelope", "", "base64(JSON) routing envelope")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	env, err := proto.DecodeEnvelope(*envArg)
	if err != nil {
		errln(stderr, "stepper: error: bad envelope: "+err.Error())
		return 1
	}
	token := os.Getenv("SLURM_SHIM_TOKEN")

	conn, err := proto.Dial(env.Dial, token, env.Host)
	if err != nil {
		errln(stderr, "stepper: error: dialing srun: "+err.Error())
		return 1
	}
	defer func() { _ = conn.Close() }()

	specFrame, err := conn.Recv()
	if err != nil || specFrame.Type != proto.FrameSpec {
		errln(stderr, "stepper: error: no StepSpec received")
		return 1
	}
	spec, err := proto.DecodeSpec(specFrame.Payload)
	if err != nil {
		errln(stderr, "stepper: error: bad StepSpec: "+err.Error())
		return 1
	}

	self, err := os.Executable()
	if err != nil {
		errln(stderr, "stepper: error: locating self: "+err.Error())
		return 1
	}

	s := &stepper{conn: conn, spec: spec, host: env.Host, self: self, stderr: stderr}
	return s.run()
}

type stepper struct {
	conn   *proto.Conn
	spec   proto.StepSpec
	host   string
	self   string
	stderr io.Writer

	mu      sync.Mutex
	procs   []*rankProc
	killSig syscall.Signal // latched terminating signal for ranks still spawning
}

type rankProc struct {
	spec proto.RankSpec
	cmd  *exec.Cmd
	pgid int
}

func (s *stepper) run() int {
	// Preflight: open every pattern output file before spawning any rank, so a
	// bad output path fails the whole host atomically (SI-21).
	files, err := s.openOutputs()
	if err != nil {
		for _, r := range s.spec.Ranks {
			_ = s.conn.Send(proto.Frame{Type: proto.FrameRankFail, Rank: uint32(r.Rank), Payload: []byte(err.Error())})
		}
		return 1
	}
	defer closeAll(files)

	_ = s.conn.Send(proto.Frame{Type: proto.FrameReady, Payload: []byte(s.host)})

	// Start the control reader (signals, liveness, srun-death detection).
	go s.readControl()

	exits := make(chan rankResult, len(s.spec.Ranks))

	for i := range s.spec.Ranks {
		r := s.spec.Ranks[i]
		if err := s.spawn(r, files[i], exits); err != nil {
			s.report(r.Rank, rankResult{fail: "spawn: " + err.Error()})
			exits <- rankResult{} // account for it so the loop terminates
		}
	}

	maxCode := 0
	for range s.spec.Ranks {
		res := <-exits
		if res.code > maxCode {
			maxCode = res.code
		}
	}
	return maxCode
}

type rankResult struct {
	code int
	fail string
}

// spawn starts one rank via the trampoline, wiring its output either to a
// pattern file or to framed output over the channel, plus a status pipe that
// distinguishes a pre-exec failure from a user exit (SI-21).
func (s *stepper) spawn(r proto.RankSpec, outFile *outputFiles, exits chan<- rankResult) error {
	rargs := []string{"rank-exec"}
	if r.Cpuset != "" {
		rargs = append(rargs, "--cpuset", r.Cpuset)
	}
	if s.spec.Chdir != "" {
		rargs = append(rargs, "--chdir", s.spec.Chdir)
	}
	rargs = append(rargs, "--")
	rargs = append(rargs, s.spec.Command...)

	cmd := exec.Command(s.self, rargs...)
	cmd.Env = rankEnv(s.spec, r, s.host)
	cmd.SysProcAttr = rankSysProcAttr()

	// Status pipe on fd 3 for pre-exec failure reporting.
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.ExtraFiles = []*os.File{statusW}

	// Output: pattern file (host-side) or framed pipe back to srun.
	var stdoutPipeR, stderrPipeR *os.File
	var pipeWriters []*os.File
	if outFile.stdout != nil {
		cmd.Stdout = outFile.stdout
	} else {
		pr, pw, e := os.Pipe()
		if e != nil {
			return e
		}
		cmd.Stdout, stdoutPipeR = pw, pr
		pipeWriters = append(pipeWriters, pw)
	}
	if outFile.stderr != nil {
		cmd.Stderr = outFile.stderr
	} else {
		pr, pw, e := os.Pipe()
		if e != nil {
			return e
		}
		cmd.Stderr, stderrPipeR = pw, pr
		pipeWriters = append(pipeWriters, pw)
	}

	if err := cmd.Start(); err != nil {
		_ = statusW.Close()
		_ = statusR.Close()
		for _, w := range pipeWriters {
			_ = w.Close()
		}
		return err
	}
	// Parent closes its copies of the child's write ends so the readers see EOF
	// when the rank exits.
	_ = statusW.Close()
	for _, w := range pipeWriters {
		_ = w.Close()
	}

	rp := &rankProc{spec: r, cmd: cmd, pgid: cmd.Process.Pid}
	s.mu.Lock()
	s.procs = append(s.procs, rp)
	pending := s.killSig // a kill may have arrived while this rank was spawning
	s.mu.Unlock()
	if pending != 0 {
		_ = syscall.Kill(-rp.pgid, pending)
	}

	// pumps tracks this rank's output goroutines so its RANK_EXIT is not sent
	// until all of its output has been framed (otherwise srun could see the exit
	// and stop before the last line arrives).
	var pumps sync.WaitGroup
	if stdoutPipeR != nil {
		pumps.Add(1)
		go func() { defer pumps.Done(); s.pump(uint32(r.Rank), 0, stdoutPipeR) }()
	}
	if stderrPipeR != nil {
		pumps.Add(1)
		go func() { defer pumps.Done(); s.pump(uint32(r.Rank), proto.FlagStderr, stderrPipeR) }()
	}

	go func() {
		reason, _ := io.ReadAll(statusR)
		_ = statusR.Close()
		err := cmd.Wait()
		pumps.Wait() // all of this rank's output is framed before its exit is reported
		if len(reason) > 0 {
			// Pre-exec failure: the command never ran.
			s.report(r.Rank, rankResult{fail: string(reason)})
			exits <- rankResult{fail: string(reason)}
			return
		}
		code := exitCode(err)
		s.report(r.Rank, rankResult{code: code})
		exits <- rankResult{code: code}
	}()
	return nil
}

// pump frames a rank's output stream back over the channel. Long lines are
// split into 64 KiB chunks with the EOL flag marking line ends, so the demux
// reproduces output byte-for-byte (REQ-RUN-020, REQ-STP-005).
func (s *stepper) pump(rank uint32, streamFlag uint8, r io.Reader) {
	br := bufio.NewReaderSize(r, proto.MaxChunk)
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			s.sendOut(rank, streamFlag|proto.FlagEOL, chunk[:len(chunk)-1])
		case errors.Is(err, bufio.ErrBufferFull):
			s.sendOut(rank, streamFlag, chunk)
		default:
			if len(chunk) > 0 {
				s.sendOut(rank, streamFlag, chunk)
			}
			return
		}
	}
}

func (s *stepper) sendOut(rank uint32, flags uint8, data []byte) {
	if len(data) == 0 && flags&proto.FlagEOL != 0 {
		// An empty but newline-terminated line still needs a frame.
		_ = s.conn.Send(proto.Frame{Type: proto.FrameOut, Rank: rank, Flags: flags})
		return
	}
	_ = s.conn.Send(proto.Frame{Type: proto.FrameOut, Rank: rank, Flags: flags, Payload: append([]byte(nil), data...)})
}

func (s *stepper) report(rank int, res rankResult) {
	if res.fail != "" {
		_ = s.conn.Send(proto.Frame{Type: proto.FrameRankFail, Rank: uint32(rank), Payload: []byte(res.fail)})
		return
	}
	_ = s.conn.Send(proto.Frame{Type: proto.FrameRankExit, Rank: uint32(rank), Payload: proto.EncodeInt32(int32(res.code))})
}

// readControl handles srun->stepper frames: signal forwarding and liveness. A
// channel error means srun died, so the stepper terminates its ranks (SI-08).
func (s *stepper) readControl() {
	for {
		f, err := s.conn.Recv()
		if err != nil {
			s.killAll(syscall.SIGTERM)
			time.Sleep(killWait)
			s.killAll(syscall.SIGKILL)
			return
		}
		switch f.Type {
		case proto.FrameSig:
			s.forwardSignal(syscall.Signal(proto.DecodeInt32(f.Payload)))
		case proto.FramePing:
			_ = s.conn.Send(proto.Frame{Type: proto.FramePong})
		}
	}
}

func (s *stepper) forwardSignal(sig syscall.Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sig == syscall.SIGTERM || sig == syscall.SIGKILL {
		// Latch it: the kill can race rank spawning under load, so a rank that
		// registers after this must be killed too (spawn checks killSig). Both
		// sides run under s.mu, so no rank can slip through unsignaled.
		s.killSig = sig
	}
	for _, p := range s.procs {
		_ = syscall.Kill(-p.pgid, sig)
	}
}

func (s *stepper) killAll(sig syscall.Signal) { s.forwardSignal(sig) }

// rankEnv layers the per-rank Table B delta and the host-local variables over
// the step base environment, deduplicating by key so a Table B value shadows
// the Table A value of the same name (B shadows A, REQ-ENV-041).
func rankEnv(spec proto.StepSpec, r proto.RankSpec, host string) []string {
	overlay := append([]string(nil), r.EnvDelta...)
	overlay = append(overlay, "SLURMD_NODENAME="+host)
	if len(r.GPUs) > 0 {
		overlay = append(overlay, "CUDA_VISIBLE_DEVICES="+joinInts(r.GPUs))
	}
	return dedupEnv(spec.Env, overlay)
}

// dedupEnv merges base then overlay, keeping the last value for each key while
// preserving first-seen order.
func dedupEnv(base, overlay []string) []string {
	idx := map[string]int{}
	var out []string
	put := func(kv string) {
		key := kv
		if eq := indexByte(kv, '='); eq >= 0 {
			key = kv[:eq]
		}
		if i, ok := idx[key]; ok {
			out[i] = kv
			return
		}
		idx[key] = len(out)
		out = append(out, kv)
	}
	for _, kv := range base {
		put(kv)
	}
	for _, kv := range overlay {
		put(kv)
	}
	return out
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func joinInts(xs []int) string {
	var b []byte
	for i, x := range xs {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, []byte(strconv.Itoa(x))...)
	}
	return string(b)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

func errln(w io.Writer, s string) { _, _ = io.WriteString(w, s+"\n") }
