package srun

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
		// Outside an allocation. --pty means "give me an interactive session":
		// translate to qrsh. SLURM_JOB_ID set (a slave host / inside a rank) is a
		// different error loadLayout already phrased, and must not become one.
		if opt.pty && os.Getenv("SLURM_JOB_ID") == "" {
			return runInteractive(cfg, opt, stderr)
		}
		errln(stderr, err.Error())
		return 1
	}
	// Inside an allocation: --pty and the allocation-shaping flags do not apply to
	// a step; run the step but say so, so a user does not think they got a session.
	if opt.pty {
		errln(stderr, "srun: warning: --pty inside an allocation runs a step with no terminal; start an interactive session from a login node")
	} else if opt.interactiveFlagsSet {
		errln(stderr, "srun: warning: --partition/--time/--mem/--gres/--account apply only to an interactive session (--pty) started outside an allocation; ignored for this step")
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
	// Refuse an unrecognised gpu.vendor once, before either path runs, so the dry
	// run cannot report a device variable the real step would refuse to write
	// (REQ-GPU-004). Scoped to steps that publish devices: config.validate only
	// warns because config.Load runs in the PE start_proc_args hook, where a fatal
	// return would reach every job on the host.
	if err := sup.resolveDeviceVars(); err != nil {
		errln(stderr, "srun: error: "+err.Error())
		return 1
	}
	sup.elastic = sup.elasticStep()
	for _, w := range sup.torchrunWarnings() {
		errln(stderr, "srun: warning: "+w)
	}
	for _, w := range sup.gpuRequestWarnings(opt) {
		errln(stderr, "srun: warning: "+w)
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
	cfg *config.Config
	opt *options
	// gpuWrite is the device-visibility variable this step publishes, and gpuDrop
	// the variables removed from the rank environment. Resolved once at dispatch
	// (resolveDeviceVars) so no later caller has to re-derive them or decide what
	// to do about an error some other caller was assumed to have handled.
	gpuWrite string
	gpuDrop  []string
	lay      *layout.Layout
	plan     *plan.StepPlan
	stepID   int
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
	killTriggered bool
	firstBadCode  int
	maxCode       int

	escalation  sync.Once   // the SIGKILL/abandon stages are armed at most once
	terminating atomic.Bool // srun is stopping the step: no spare may replace a node

	// conns are the open stepper channels. A hot-spare relaunch adds one while
	// the step runs, so they have their own lock. Lock order: mu, then connsMu.
	connsMu sync.Mutex
	conns   []*proto.Conn

	// Hot spares (elastic.go). elastic is decided once per step; the launch
	// context below is kept so a spare can be launched mid-step exactly like
	// the original steppers.
	elastic           bool
	softSwapped       bool // a stepper ended early (not a lost host) and took a spare
	swapRecordWarning sync.Once
	recordMu          sync.Mutex     // serializes recordSwaps
	bg                sync.WaitGroup // swap side effects (drain, recordSwaps)
	runner            gedata.Runner  // nil: gedata.ExecRunner (tests inject a fake)
	srv               *proto.Server
	token             string
	slave             launch.Launcher
	remote            bool
	handles           []launch.Handle // guarded by mu

	// Signal state across the launch (signals.go). Until connected, a received
	// signal cannot be forwarded: the first terminating one is kept in
	// interruptSig and closes interrupted, so the launch aborts.
	connected    bool
	interruptSig syscall.Signal
	interrupted  chan struct{}
}

// killEscalation is the interval between the stages of a kill-on-bad-exit
// fan-out: SIGTERM, then SIGKILL over the control channel, then abandoning the
// steppers that still have not reported (SLURM's UnkillableStepTimeout
// analogue), so a rank that refuses to die cannot hang srun.
//
// srun never kills a stepper's launcher handle. Under tight integration that
// handle is the local `qrsh -inherit` client, and killing it makes the remote pe
// task die by signal, which qmaster answers by deleting the whole job (failed
// 100), taking the rest of the batch script with it. Every stage therefore works
// through the control channel, and the stepper always exits with a code.
//
// A var, not a const, so tests can shorten it.
var killEscalation = 10 * time.Second

// stepperExitWait bounds how long srun waits for the stepper processes to exit
// once every rank has reported or been abandoned. A stepper on an unreachable
// host never exits; srun leaves it to Grid Engine's job cleanup rather than
// hanging.
var stepperExitWait = 15 * time.Second

func (s *supervisor) launch() int {
	token, err := proto.NewToken()
	if err != nil {
		errln(s.stderr, "srun: error: token: "+err.Error())
		return 1
	}

	master, slave, err := launchersFor(s)
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

	srv, err := s.listen(remote, token)
	if err != nil {
		errln(s.stderr, "srun: error: opening control channel: "+err.Error())
		return exitLauncher
	}
	defer func() { _ = srv.Close() }()
	defer s.bg.Wait()
	s.srv, s.token, s.slave, s.remote = srv, token, slave, remote

	// Preflight tight-integration launch before spawning anything (REQ-CHN-005).
	if remote {
		pf := launch.Preflight(context.Background(), gedata.ExecRunner{}, s.lay.Job.PEName, s.lay.Job.Queue)
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

	// From here on, srun must not die by a signal's default action (signals.go).
	s.interrupted = make(chan struct{})
	s.installSignals()

	// Accept while launching: a connected stepper is held only briefly until
	// srun accepts it, and starting many nodes takes longer than that. The
	// launch_timeout deadline is armed once the last start returned.
	conns := map[string]*proto.Conn{}
	acceptCtx, stopAccept := context.WithCancel(context.Background())
	defer stopAccept()
	acceptErr := make(chan error, 1)
	go func() { acceptErr <- acceptSteppers(acceptCtx, srv, len(s.plan.Nodes), conns) }()
	// An interrupt ends the wait for connections. Cancelling this context is
	// safe: it only bounds Accept, never a launched qrsh client.
	go func() {
		select {
		case <-s.interrupted:
			stopAccept()
		case <-acceptCtx.Done():
		}
	}()

	handles, ok := s.startSteppers(master, slave, srv, token, remote)
	if !ok {
		stopAccept()
		<-acceptErr
		s.abortLaunch(srv, conns, handles)
		if sig := s.launchInterrupted(); sig != 0 {
			return 128 + int(sig)
		}
		return exitLauncher
	}
	deadline := time.AfterFunc(s.cfg.LaunchTimeout.Duration, stopAccept)
	err = <-acceptErr
	deadline.Stop()
	if err != nil {
		s.abortLaunch(srv, conns, handles)
		if sig := s.launchInterrupted(); sig != 0 {
			return 128 + int(sig)
		}
		errln(s.stderr, "srun: error: stepper did not connect within launch_timeout")
		if remote {
			errln(s.stderr, "srun: the control channel is listening on "+srv.Addr()+
				"; if this cluster filters traffic between nodes, that port range must be "+
				"open to this host (run `slurm-shim ports` for the exact rules)")
		}
		return exitLauncher
	}

	// Push each host's StepSpec.
	base := s.baseEnv()
	for ni, node := range s.plan.Nodes {
		spec := s.stepSpec(base, ni)
		payload, err := proto.EncodeSpec(spec)
		if err != nil {
			errln(s.stderr, "srun: error: encoding spec: "+err.Error())
			s.abortLaunch(srv, conns, handles)
			return 1
		}
		c := conns[node.Host]
		if c == nil {
			errln(s.stderr, "srun: error: no stepper connected for "+node.Host)
			s.abortLaunch(srv, conns, handles)
			return exitLauncher
		}
		if err := c.Send(proto.Frame{Type: proto.FrameSpec, Payload: payload}); err != nil {
			errln(s.stderr, "srun: error: sending spec: "+err.Error())
			s.abortLaunch(srv, conns, handles)
			return exitLauncher
		}
	}

	// A signal that arrived before this point is not forwarded: the steppers may
	// not have spawned their ranks yet, and a SIGINT sent then would be lost
	// (the stepper latches only SIGTERM/SIGKILL for ranks still spawning, and
	// that was observed live). Abort instead: the closed channels make every
	// stepper terminate whatever it started and exit with a code. The check and
	// `connected` change under one lock, so no signal falls between them.
	s.mu.Lock()
	pending := s.interruptSig
	if pending == 0 {
		for _, c := range conns {
			s.addConn(c)
		}
		s.connected = true
		s.handles = append(s.handles, handles...)
	}
	s.mu.Unlock()
	if pending != 0 {
		s.abortLaunch(srv, conns, handles)
		return 128 + int(pending)
	}

	s.demux = mux.NewDemux(s.stdout, s.stderr, s.opt.label)
	code := s.supervise(conns)

	_ = s.demux.Flush()
	s.mu.Lock()
	all := append([]launch.Handle(nil), s.handles...)
	s.mu.Unlock()
	s.waitHandles(all)
	return code
}

// launchConcurrency bounds the steppers being started at once. Every qrsh start
// waits out qrsh's 2s rejection window, so one at a time costs 2s per node; 64
// at a time keeps a 1000-node step near half a minute without flooding the
// master host's forks or qmaster. A var so tests can lower it.
var launchConcurrency = 64

// launchersFor returns the launchers for the master host -- always local, so
// its ranks go through a stepper too (REQ-RUN-012) -- and for the slave hosts
// (the configured backend). A var so tests can launch fake steppers.
var launchersFor = func(s *supervisor) (master, slave launch.Launcher, err error) {
	slave, err = launch.For(s.cfg, s.self, s.stderr)
	return launch.LocalLauncher{Self: s.self, Stderr: s.stderr}, slave, err
}

// startSteppers starts one stepper per step node, at most launchConcurrency at
// once, and returns the handles of those that started; false means abort. A
// start's context is never cancelled -- a qrsh client ended by a signal makes
// Grid Engine delete the job -- so after a failure or an interrupt only new
// starts are skipped. Nodes whose host refused the task move to spares
// afterwards, one at a time: the swap bookkeeping must not run concurrently.
func (s *supervisor) startSteppers(master, slave launch.Launcher, srv *proto.Server, token string, remote bool) ([]launch.Handle, bool) {
	start := func(ni int) (launch.Handle, error) {
		node := s.plan.Nodes[ni]
		launcher := master
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
		}
		return h, err
	}

	handles := make([]launch.Handle, len(s.plan.Nodes))
	errs := make([]error, len(s.plan.Nodes))
	sem := make(chan struct{}, launchConcurrency)
	var wg sync.WaitGroup
	var failed atomic.Bool
	for ni := range s.plan.Nodes {
		select {
		case sem <- struct{}{}:
		case <-s.interrupted:
		}
		if failed.Load() || s.launchInterrupted() != 0 {
			break
		}
		wg.Add(1)
		go func(ni int) {
			defer wg.Done()
			defer func() { <-sem }()
			handles[ni], errs[ni] = start(ni)
			if errs[ni] != nil && !s.hostFault(ni, errs[ni]) {
				failed.Store(true)
			}
		}(ni)
	}
	wg.Wait()
	for ni, err := range errs {
		if err == nil || failed.Load() || s.launchInterrupted() != 0 {
			continue
		}
		h, err := s.startOnSpare(ni, err, start)
		if err != nil {
			failed.Store(true)
			continue
		}
		handles[ni] = h
	}
	var started []launch.Handle
	for _, h := range handles {
		if h != nil {
			started = append(started, h)
		}
	}
	return started, !failed.Load() && s.launchInterrupted() == 0
}

// acceptSteppers fills conns until n distinct hosts are connected or ctx ends.
// A second connection from one host is closed; its stepper exits with a code.
func acceptSteppers(ctx context.Context, srv *proto.Server, n int, conns map[string]*proto.Conn) error {
	for len(conns) < n {
		c, err := srv.Accept(ctx)
		if err != nil {
			return err
		}
		if _, dup := conns[c.Host]; dup {
			_ = c.Close()
			continue
		}
		conns[c.Host] = c
	}
	return nil
}

// addConn registers an open stepper channel for broadcasts and abandon.
func (s *supervisor) addConn(c *proto.Conn) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	s.conns = append(s.conns, c)
}

// addLiveConn registers a channel opened mid-step (a hot-spare relaunch) unless
// srun is already stopping the step. Checked under connsMu, so it cannot slip in
// after abandon took its snapshot of the channels to close.
func (s *supervisor) addLiveConn(c *proto.Conn) bool {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.terminating.Load() {
		return false
	}
	s.conns = append(s.conns, c)
	return true
}

// allConns is a snapshot of the open stepper channels.
func (s *supervisor) allConns() []*proto.Conn {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	return append([]*proto.Conn(nil), s.conns...)
}

// addHandle registers a stepper launched mid-step (a hot-spare relaunch).
func (s *supervisor) addHandle(h launch.Handle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handles = append(s.handles, h)
}

// abortLaunch ends a step that failed before supervision started. Closing the
// listener and the accepted channels makes every started stepper exit with a
// code (a stepper still dialling is refused; a connected one loses its channel;
// an authenticated one nobody accepted is dropped after proto.HelloTimeout, which is
// shorter than stepperExitWait). srun then waits for them, so no qrsh client is
// left behind writing its stderr into the pipe of an srun that has exited: a
// SIGPIPE there would kill the client, and the remote pe task with it.
func (s *supervisor) abortLaunch(srv *proto.Server, conns map[string]*proto.Conn, handles []launch.Handle) {
	_ = srv.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.waitHandles(handles)
}

// waitHandles waits for the stepper processes to exit, at most stepperExitWait,
// and names the hosts whose stepper is still running when it gives up.
func (s *supervisor) waitHandles(handles []launch.Handle) {
	done := make([]chan struct{}, len(handles))
	for i, h := range handles {
		done[i] = make(chan struct{})
		go func(h launch.Handle, d chan struct{}) { _ = h.Wait(); close(d) }(h, done[i])
	}
	deadline := time.After(stepperExitWait)
	for i := range handles {
		select {
		case <-done[i]:
		case <-deadline:
			var left []string
			for j := i; j < len(handles); j++ {
				select {
				case <-done[j]:
				default:
					left = append(left, handles[j].Host())
				}
			}
			errln(s.stderr, fmt.Sprintf("srun: warning: stepper on %s did not exit within %s; "+
				"its task keeps its slot until the job's cleanup", strings.Join(left, ","), stepperExitWait))
			return
		}
	}
}

// watchRemoteHosts lets the kernel detect a remote stepper's host going away
// (REQ-CHN-004 as amended): a channel to a crashed, powered-off or partitioned
// host fails with a timeout after ping_deadline, and supervise counts that
// host's ranks as failed. A suspended or merely slow stepper keeps its host
// answering, so it is never mistaken for a lost one. The master's own stepper
// is local: losing that host means losing srun itself.
func (s *supervisor) watchRemoteHosts(conns map[string]*proto.Conn) {
	if s.cfg.PingDeadline.Duration <= 0 || s.cfg.PingInterval.Duration <= 0 {
		return
	}
	for _, n := range s.plan.Nodes {
		if c := conns[n.Host]; c != nil && n.LayoutIndex != 0 {
			if err := c.SetLiveness(s.cfg.PingDeadline.Duration, s.cfg.PingInterval.Duration); err != nil {
				errln(s.stderr, fmt.Sprintf("srun: warning: cannot watch node %s for loss: %v", n.Host, err))
			}
		}
	}
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
		lost bool // the channel timed out: the stepper's host stopped answering
	}
	s.watchRemoteHosts(conns)

	// Readers feed one channel. The loop runs until every rank has reported, not
	// until the channel closes: a hot-spare relaunch adds a reader mid-step, so
	// "all readers done" is not a stable end.
	events := make(chan event, 64)
	read := func(host string, c *proto.Conn) {
		go func() {
			for {
				f, err := c.Recv()
				if err != nil {
					events <- event{host: host, eof: true, lost: proto.IsLivenessTimeout(err)}
					return
				}
				events <- event{host: host, f: f}
			}
		}()
	}
	for host, c := range conns {
		read(host, c)
	}

	hostReported := map[string]int{}
	for reported < total {
		ev := <-events
		if ev.eof {
			if hostReported[ev.host] >= perHostExpected[ev.host] {
				continue // finished (or already replaced): nothing outstanding
			}
			// An elastic step answers a lost node with a spare: same tasks, same
			// node index, nothing counted as failed (elastic.go).
			if s.canReplace(ev.host, hostReported[ev.host]) {
				if spare, c, ok := s.replace(ev.host, ev.lost); ok {
					perHostExpected[spare] = perHostExpected[ev.host]
					delete(perHostExpected, ev.host)
					read(spare, c)
					continue
				}
			} else if ev.lost {
				errln(s.stderr, fmt.Sprintf("srun: error: lost node %s: it stopped answering for %s; "+
					"its tasks are treated as failed", ev.host, s.cfg.PingDeadline.Duration))
			}
			// A stepper closed before reporting all its ranks: synthesize
			// failures for the missing ones (REQ-RUN-027, SI-08).
			for hostReported[ev.host] < perHostExpected[ev.host] {
				hostReported[ev.host]++
				reported++
				s.recordExit(1)
				if !ev.lost {
					errln(s.stderr, fmt.Sprintf("srun: error: stepper on %s exited without reporting a rank", ev.host))
				}
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
			s.armEscalation()
		}
	}
}

// armEscalation bounds a termination that srun started (kill-on-bad-exit, or a
// forwarded SIGTERM/SIGHUP): a rank that ignores SIGTERM gets SIGKILL, and a
// stepper that still does not report is abandoned (killEscalation).
func (s *supervisor) armEscalation() {
	s.terminating.Store(true)
	s.escalation.Do(func() { time.AfterFunc(killEscalation, s.escalate) })
}

// escalate is the second kill-on-bad-exit stage: SIGKILL over the control
// channel, which the stepper forwards to its ranks' process groups before
// reporting their exits and exiting with a code. The third stage follows.
func (s *supervisor) escalate() {
	s.broadcast(proto.Frame{Type: proto.FrameSig, Payload: proto.EncodeInt32(int32(syscallSIGKILL))})
	time.AfterFunc(killEscalation, s.abandon)
}

// abandon is the last kill-on-bad-exit stage: close every control channel.
// supervise observes the closed channels as EOF and synthesizes the missing
// ranks' exits; a stepper that is still alive sees its channel drop, stops its
// ranks, and exits with a code. A channel whose stepper already finished is
// closed harmlessly.
func (s *supervisor) abandon() {
	for _, c := range s.allConns() {
		_ = c.Close()
	}
}

// broadcast sends f to every stepper. recordExit calls it while holding s.mu,
// which is fine: allConns takes only connsMu (lock order mu, then connsMu).
func (s *supervisor) broadcast(f proto.Frame) {
	for _, c := range s.allConns() {
		_ = c.Send(f)
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

func (s *supervisor) bindHost(remote bool) string {
	// Loopback unless steppers run off-box: then bind all interfaces so remote
	// steppers can reach the control channel. It is token-authenticated
	// (REQ-CHN-002), so exposure past loopback is safe.
	if remote {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// listen opens the control channel. A remote step binds inside the configured
// port range so the site can admit it with one firewall rule; a loopback step
// needs no rule and keeps an ephemeral port, which avoids consuming the range
// for steps nothing outside the box can reach.
func (s *supervisor) listen(remote bool, token string) (*proto.Server, error) {
	if !remote {
		return proto.Listen(s.bindHost(false)+":0", token)
	}
	return proto.ListenRange(s.bindHost(true), s.cfg.ControlPortBase, s.cfg.ControlPortRange, token)
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
