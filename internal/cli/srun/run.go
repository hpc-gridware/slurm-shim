package srun

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/mux"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
	"github.com/hpc-gridware/slurm-shim/internal/version"
)

// exitLauncher is the launcher-failure exit code (sec. 12.1, SI-17).
const exitLauncher = 8

// Run is the srun entry point.
func Run(args []string, stdout, stderr io.Writer) int {
	cfg, warns, err := config.Load()
	if err != nil {
		errln(stderr, "srun: error: loading config: "+err.Error())
		return 1
	}
	for _, w := range warns {
		errln(stderr, "srun: warning: "+w)
	}

	opt, err := parseFlags(args, cfg.StrictFlags, stderr)
	if err != nil {
		errln(stderr, err.Error())
		return 1
	}
	if opt.version {
		fmt.Fprintln(stdout, version.String(cfg.CompatVersion))
		return 0
	}
	// --gpu-bind / SLURM_GPU_BIND / gpu.bind decide whether tasks are bound to a
	// device subset; resolve before placement, and before warnings are drained.
	applyGPUBind(opt, cfg, os.Getenv("SLURM_GPU_BIND"))
	warnCgroupCannotBind(opt, cfg)
	for _, w := range opt.warnings {
		fmt.Fprintf(stderr, "srun: warning: %s\n", w)
	}
	if len(opt.command) == 0 {
		errln(stderr, "srun: error: no command given")
		return 1
	}

	lay, err := loadLayout(cfg, stderr)
	if err != nil {
		errln(stderr, err.Error())
		return 1
	}

	p, err := plan.Place(lay, opt.req)
	if err != nil {
		errln(stderr, err.Error())
		return 1
	}
	for _, w := range p.Warnings {
		errln(stderr, w)
	}

	// A dry run must not consume a step id: the step it describes is the one the
	// next real srun will create. Both readers surface the same failures, so a
	// counter that would abort the real step aborts the report too.
	// loadLayout above already refused an unset TMPDIR, so this cannot fail in
	// practice; surface it rather than silently joining a relative path.
	sdir, err := stateDir()
	if err != nil {
		errln(stderr, "srun: error: "+err.Error())
		return 1
	}
	ctr := filepath.Join(sdir, layout.StepCtrFile)
	dry := dryrun.Enabled() || opt.testOnly
	var stepID int
	if dry {
		stepID, err = layout.PeekStep(ctr)
	} else {
		stepID, err = layout.NextStep(ctr)
	}
	if err != nil {
		errln(stderr, "srun: error: reserving step id: "+err.Error())
		return 1
	}

	arrayJob := lay.Job.JobID
	if v, err := strconv.ParseInt(os.Getenv("SLURM_ARRAY_JOB_ID"), 10, 64); err == nil {
		arrayJob = v
	}
	var arrayTask int64
	if v, err := strconv.ParseInt(os.Getenv("SLURM_ARRAY_TASK_ID"), 10, 64); err == nil {
		arrayTask = v
	}
	user := lay.Job.User
	if user == "" {
		user = os.Getenv("USER")
	}

	// The absolute shim path is the stepper argv[0] on every host, and the dry run
	// reports it, so both resolve it the same way and at the same point.
	self, err := os.Executable()
	if err != nil {
		errln(stderr, "srun: error: locating self: "+err.Error())
		return 1
	}

	sup := &supervisor{
		cfg:         cfg,
		opt:         opt,
		lay:         lay,
		plan:        p,
		stepID:      stepID,
		self:        self,
		stdout:      stdout,
		stderr:      stderr,
		kill:        resolveKill(opt.killFlag, cfg),
		arrayJobID:  arrayJob,
		arrayTaskID: arrayTask,
		user:        user,
	}
	if dry {
		if v := dryrun.Unrecognized(); v != "" {
			errln(stderr, fmt.Sprintf("srun: warning: %s=%q is not a recognized on/off value; treating as off",
				dryrun.EnvVar, v))
		}
		return sup.dryRun()
	}
	if v := dryrun.Unrecognized(); v != "" {
		errln(stderr, fmt.Sprintf("srun: warning: %s=%q is not a recognized on/off value; launching for real",
			dryrun.EnvVar, v))
	}
	return sup.launch()
}

type supervisor struct {
	cfg    *config.Config
	opt    *options
	lay    *layout.Layout
	plan   *plan.StepPlan
	stepID int
	// self is the absolute shim path used as the stepper's argv[0] on every host.
	self   string
	stdout io.Writer
	stderr io.Writer
	kill   bool

	// SLURM-facing coordinates for output-pattern expansion (%A/%a/%u), resolved
	// once from the fabricated job env + layout rather than re-read per rank.
	arrayJobID  int64
	arrayTaskID int64
	user        string

	demux *mux.Demux

	mu            sync.Mutex
	conns         []*proto.Conn
	handles       []launch.Handle
	killTriggered bool
	firstBadCode  int
	maxCode       int
}

// killEscalation bounds how long srun waits for a kill-on-bad-exit fan-out to
// drain before force-killing the stepper handles, so an unkillable or slow-to-die
// rank cannot hang srun indefinitely (SLURM's UnkillableStepTimeout analogue).
// A var, not a const, so tests can shorten it.
var killEscalation = 10 * time.Second

func (s *supervisor) launch() int {
	token, err := proto.NewToken()
	if err != nil {
		errln(s.stderr, "srun: error: token: "+err.Error())
		return 1
	}

	// The master host always runs under the LocalLauncher (REQ-RUN-012); slave
	// hosts use the configured backend (qrsh-inherit by default).
	master := launch.LocalLauncher{Self: s.self, Stderr: s.stderr}
	slave, err := launch.For(s.cfg, s.self, s.stderr)
	if err != nil {
		errln(s.stderr, "srun: error: "+err.Error())
		return exitLauncher
	}
	// remote is true only when a stepper actually runs off the master: a real
	// tight-integration launcher placing a task on a non-master node. That is
	// precisely when the control channel must be reachable past loopback. Gate on
	// the resolved slave launcher, not the config string, so it cannot drift from
	// the factory's own selection (e.g. an empty launcher value). The local
	// launcher keeps everything on loopback even for a multi-node layout (D-6, and
	// the test suite).
	_, slaveIsQrsh := slave.(launch.QrshLauncher)
	remote := s.stepIsRemote(slaveIsQrsh)

	srv, err := proto.Listen(s.bindAddr(remote), token)
	if err != nil {
		errln(s.stderr, "srun: error: opening control channel: "+err.Error())
		return exitLauncher
	}
	defer func() { _ = srv.Close() }()

	// Preflight tight-integration launch before spawning anything (REQ-CHN-005).
	if remote {
		pf := launch.Preflight(context.Background(), gedata.ExecRunner{}, s.lay.Job.PEName)
		for _, w := range pf.Warnings {
			errln(s.stderr, "srun: warning: "+w)
		}
		if !pf.OK() {
			for _, e := range pf.Errors {
				errln(s.stderr, "srun: error: "+e)
			}
			return exitLauncher
		}
	}

	// Launch one stepper per participating host. The master host (layout index 0)
	// always launches locally (REQ-RUN-012); it need not be step-node 0 when -w
	// selects a subset that still includes it.
	var handles []launch.Handle
	for ni, node := range s.plan.Nodes {
		launcher := launch.Launcher(master)
		if node.LayoutIndex != 0 {
			launcher = slave
		}
		env := proto.Envelope{
			JobID:  s.lay.Job.JobID,
			StepID: s.stepID,
			Host:   node.Host,
			NodeID: ni,
			Dial:   s.dialAddr(srv.Addr(), remote),
		}
		h, err := launcher.Start(context.Background(), node.Host, env, token)
		if err != nil {
			errln(s.stderr, fmt.Sprintf("srun: error: launching stepper on %s: %v", node.Host, err))
			s.killHandles(handles)
			return exitLauncher
		}
		handles = append(handles, h)
	}

	// Accept each stepper's authenticated connection within launch_timeout.
	conns := map[string]*proto.Conn{}
	acceptCtx, cancel := context.WithTimeout(context.Background(), s.cfg.LaunchTimeout.Duration)
	defer cancel()
	for range s.plan.Nodes {
		c, err := srv.Accept(acceptCtx)
		if err != nil {
			errln(s.stderr, "srun: error: stepper did not connect within launch_timeout")
			s.killHandles(handles)
			return exitLauncher
		}
		conns[c.Host] = c
	}

	// Push each host's StepSpec.
	base := s.baseEnv()
	for ni, node := range s.plan.Nodes {
		spec := s.stepSpec(base, ni)
		payload, err := proto.EncodeSpec(spec)
		if err != nil {
			errln(s.stderr, "srun: error: encoding spec: "+err.Error())
			return 1
		}
		if err := conns[node.Host].Send(proto.Frame{Type: proto.FrameSpec, Payload: payload}); err != nil {
			errln(s.stderr, "srun: error: sending spec: "+err.Error())
			return exitLauncher
		}
	}

	s.mu.Lock()
	for _, c := range conns {
		s.conns = append(s.conns, c)
	}
	s.handles = handles // for the kill-escalation watchdog
	s.mu.Unlock()

	s.demux = mux.NewDemux(s.stdout, s.stderr, s.opt.label)
	code := s.supervise(conns)

	_ = s.demux.Flush()
	for _, h := range handles {
		_ = h.Wait()
	}
	return code
}

// supervise reads frames from every stepper connection until all ranks have
// reported, aggregating output and exit codes.
func (s *supervisor) supervise(conns map[string]*proto.Conn) int {
	total := len(s.plan.Ranks)
	reported := 0
	perHostExpected := s.ranksPerHost()

	type event struct {
		host string
		f    proto.Frame
		eof  bool
	}
	events := make(chan event, 64)
	var readers sync.WaitGroup
	for host, c := range conns {
		readers.Add(1)
		go func(host string, c *proto.Conn) {
			defer readers.Done()
			for {
				f, err := c.Recv()
				if err != nil {
					events <- event{host: host, eof: true}
					return
				}
				events <- event{host: host, f: f}
			}
		}(host, c)
	}
	go func() { readers.Wait(); close(events) }()

	s.installSignals(conns)

	hostReported := map[string]int{}
	for ev := range events {
		if ev.eof {
			// A stepper closed before reporting all its ranks: synthesize
			// failures for the missing ones (REQ-RUN-027, SI-08).
			for hostReported[ev.host] < perHostExpected[ev.host] {
				hostReported[ev.host]++
				reported++
				s.recordExit(1)
				errln(s.stderr, fmt.Sprintf("srun: error: stepper on %s exited without reporting a rank", ev.host))
			}
			if reported >= total {
				break
			}
			continue
		}
		switch ev.f.Type {
		case proto.FrameOut:
			_ = s.demux.Handle(ev.f)
		case proto.FrameRankExit:
			hostReported[ev.host]++
			reported++
			s.recordExit(int(proto.DecodeInt32(ev.f.Payload)))
		case proto.FrameRankFail:
			hostReported[ev.host]++
			reported++
			errln(s.stderr, fmt.Sprintf("srun: error: task %d failed to start: %s", ev.f.Rank, ev.f.Payload))
			s.recordExit(1)
		}
		if reported >= total {
			break
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.killTriggered {
		return s.firstBadCode
	}
	return s.maxCode
}

// recordExit aggregates one rank's exit code (SI-07): the running max for
// normal completion, and the first organically-failing code plus a kill fan-out
// when kill-on-bad-exit is active.
func (s *supervisor) recordExit(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code > s.maxCode {
		s.maxCode = code
	}
	if code != 0 && !s.killTriggered {
		s.firstBadCode = code
		if s.kill {
			s.killTriggered = true
			errln(s.stderr, fmt.Sprintf("srun: error: task exited with code %d, killing remaining tasks (kill-on-bad-exit)", code))
			s.broadcast(proto.Frame{Type: proto.FrameSig, Payload: proto.EncodeInt32(int32(syscallSIGTERM))})
			// Bound the wait: if a rank refuses to die, force-kill the stepper
			// handles so supervise (and h.Wait) unblock instead of hanging forever.
			time.AfterFunc(killEscalation, s.forceKill)
		}
	}
}

// forceKill terminates the stepper handles. supervise then observes the closed
// control channels as EOF and synthesizes the missing ranks' exits, and launch's
// h.Wait returns because the processes are gone.
func (s *supervisor) forceKill() {
	s.mu.Lock()
	handles := s.handles
	s.mu.Unlock()
	s.killHandles(handles)
}

func (s *supervisor) broadcast(f proto.Frame) {
	for _, c := range s.conns {
		_ = c.Send(f)
	}
}

func (s *supervisor) killHandles(handles []launch.Handle) {
	for _, h := range handles {
		_ = h.Kill()
	}
}

// stepIsRemote reports whether any stepper in this step runs off the master
// node: a real tight-integration launcher (not the local one) placing a task on
// a node other than layout index 0. Only then must the control channel be
// routable. This is a superset of "more than one node" -- a single-node step
// selected with `-w <worker>` also runs off-box, since its one node carries a
// non-zero layout index and the launch loop hands it to the slave launcher.
func (s *supervisor) stepIsRemote(slaveIsQrsh bool) bool {
	return slaveIsQrsh && s.hasSlaveNode()
}

func (s *supervisor) bindAddr(remote bool) string {
	// Loopback unless steppers run off-box: then bind all interfaces so remote
	// steppers can reach the control channel. It is token-authenticated
	// (REQ-CHN-002), so exposure past loopback is safe.
	if remote {
		return "0.0.0.0:0"
	}
	return "127.0.0.1:0"
}

// dialAddr is the address steppers are told to connect back to. Loopback jobs
// (single-node, or any local-launcher run) keep the listener address. A remote
// job replaces the wildcard bind host with the master's routable address (the
// same host the job's MASTER_ADDR advertises) while keeping the listener's
// ephemeral port, so a stepper launched on a remote node reaches srun instead of
// its own loopback.
func (s *supervisor) dialAddr(listen string, remote bool) string {
	if !remote {
		return listen
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	host := s.lay.Rendezvous.MasterAddr
	if host == "" {
		host = s.lay.Nodes[0].Host
	}
	return net.JoinHostPort(host, port)
}

func (s *supervisor) ranksPerHost() map[string]int {
	m := map[string]int{}
	for _, r := range s.plan.Ranks {
		m[s.plan.Nodes[r.StepNodeIndex].Host]++
	}
	return m
}

func errln(w io.Writer, s string) { fmt.Fprintln(w, s) }

func resolveKill(flag string, cfg *config.Config) bool {
	// Precedence: -K flag > SLURM_KILL_BAD_EXIT env > config > default (SI-42/59).
	if flag != "" {
		return flag != "0"
	}
	if v, ok := os.LookupEnv("SLURM_KILL_BAD_EXIT"); ok {
		return v != "0"
	}
	return cfg.KillOnBadExit
}

func loadLayout(cfg *config.Config, stderr io.Writer) (*layout.Layout, error) {
	dir, err := stateDir()
	if err != nil {
		// No TMPDIR means no allocation to be inside of, which is exactly the
		// standalone case this function already reports below.
		return nil, fmt.Errorf("srun: error: not inside a slurm-shim allocation (standalone: %s)", cfg.Standalone)
	}
	path := filepath.Join(dir, layout.LayoutFile)
	lay, err := layout.Read(path)
	if err == nil {
		return lay, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("srun: error: reading layout: %w", err)
	}
	// No layout. On a slave host inside a job the message is specific (SI-28).
	if job := os.Getenv("SLURM_JOB_ID"); job != "" {
		return nil, fmt.Errorf("srun: error: srun must run on the master host of job %s", job)
	}
	return nil, fmt.Errorf("srun: error: not inside a slurm-shim allocation (standalone: %s)", cfg.Standalone)
}

// stateDir resolves the per-job state directory, refusing an unset TMPDIR
// rather than reading allocation truth from a shared /tmp path (REQ-FAB-010).
func stateDir() (string, error) {
	return layout.StateDirFor(os.Getenv("TMPDIR"))
}

// stepPerNode returns the step-scoped per-node task counts in step order.
func (s *supervisor) stepPerNode() []int {
	counts := make([]int, len(s.plan.Nodes))
	for _, r := range s.plan.Ranks {
		counts[r.StepNodeIndex]++
	}
	return counts
}

func joinHosts(nodes []plan.StepNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Host
	}
	return out
}
