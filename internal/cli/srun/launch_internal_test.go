package srun

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// countingLauncher records how many starts run at once, which hosts were
// started, and whether any start's context could ever be cancelled.
type countingLauncher struct {
	delay     time.Duration
	barrier   int // each Start waits until this many were in flight at once (bounded)
	fail      map[string]error
	mu        sync.Mutex
	inFlight  int
	maxFlight int
	started   []string
	cancel    atomic.Bool
}

func (l *countingLauncher) Start(ctx context.Context, host string, _ proto.Envelope, _ string) (launch.Handle, error) {
	if ctx.Done() != nil {
		l.cancel.Store(true) // a cancellable context could SIGKILL a qrsh client
	}
	l.mu.Lock()
	l.inFlight++
	if l.inFlight > l.maxFlight {
		l.maxFlight = l.inFlight
	}
	l.started = append(l.started, host)
	l.mu.Unlock()
	for deadline := time.Now().Add(2 * time.Second); l.barrier > 0 && time.Now().Before(deadline); {
		l.mu.Lock()
		reached := l.maxFlight >= l.barrier
		l.mu.Unlock()
		if reached {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(l.delay)
	l.mu.Lock()
	l.inFlight--
	l.mu.Unlock()
	if err := l.fail[host]; err != nil {
		return nil, err
	}
	return exitedHandle(host), nil
}

func launchSupervisor(n int) *supervisor {
	nodes := make([]plan.StepNode, n)
	ranks := make([]plan.PlacedRank, n)
	for i := range nodes {
		nodes[i] = plan.StepNode{Host: fmt.Sprintf("node%03d", i+1), LayoutIndex: i}
		ranks[i] = plan.PlacedRank{Rank: i, StepNodeIndex: i}
	}
	return &supervisor{
		cfg: config.Default(), opt: &options{}, stderr: gbytes.NewBuffer(),
		lay:         &layout.Layout{},
		plan:        &plan.StepPlan{Nodes: nodes, Ranks: ranks},
		interrupted: make(chan struct{}),
	}
}

var _ = Describe("srun: starting steppers in parallel", func() {
	var srv *proto.Server
	BeforeEach(func() {
		var err error
		srv, err = proto.Listen("127.0.0.1:0", "tok")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(srv.Close)
		orig := launchConcurrency
		DeferCleanup(func() { launchConcurrency = orig })
	})

	It("starts every node with at most launchConcurrency in flight, never with a cancellable context", func() {
		launchConcurrency = 3
		l := &countingLauncher{barrier: 3}
		handles, ok := launchSupervisor(10).startSteppers(l, l, srv, "tok", false)
		Expect(ok).To(BeTrue())
		Expect(handles).To(HaveLen(10))
		Expect(l.maxFlight).To(Equal(3))
		Expect(l.cancel.Load()).To(BeFalse())
	})

	It("starts 64 nodes in one wave at the default bound", func() {
		l := &countingLauncher{barrier: 64}
		handles, ok := launchSupervisor(64).startSteppers(l, l, srv, "tok", false)
		Expect(ok).To(BeTrue())
		Expect(handles).To(HaveLen(64))
		Expect(l.maxFlight).To(Equal(64))
	})

	It("starts nothing new after a failure, and returns what started for abortLaunch", func() {
		launchConcurrency = 1
		l := &countingLauncher{fail: map[string]error{"node003": errors.New("qrsh: misconfigured")}}
		handles, ok := launchSupervisor(10).startSteppers(l, l, srv, "tok", false)
		Expect(ok).To(BeFalse())
		Expect(l.started).To(Equal([]string{"node001", "node002", "node003"}))
		Expect(handles).To(HaveLen(2))
	})

	It("starts nothing once the launch was interrupted", func() {
		s := launchSupervisor(4)
		s.interruptSig = syscall.SIGINT
		close(s.interrupted)
		l := &countingLauncher{}
		_, ok := s.startSteppers(l, l, srv, "tok", false)
		Expect(ok).To(BeFalse())
		Expect(l.started).To(BeEmpty())
	})

	It("moves two nodes lost at the same time onto two different spares", func() {
		tmp := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		Expect(layout.Write(filepath.Join(tmp, layout.StateDir), &layout.Layout{
			SchemaVersion: layout.SchemaVersion,
			Nodes: []layout.Node{{Index: 0, Host: "node001", IsMaster: true}, {Index: 1, Host: "node002"},
				{Index: 2, Host: "node003"}, {Index: 3, Host: "node004"}},
			Spares: []layout.Node{{Host: "node005"}, {Host: "node006"}},
		})).To(Succeed())
		launchConcurrency = 4
		down := launch.HostError(errors.New("never accepted the task"))
		l := &countingLauncher{delay: 20 * time.Millisecond, fail: map[string]error{"node002": down, "node003": down}}
		s := launchSupervisor(4)
		s.elastic = true
		handles, ok := s.startSteppers(l, l, srv, "tok", false)
		Expect(ok).To(BeTrue())
		Expect(handles).To(HaveLen(4))
		hosts := []string{s.plan.Nodes[1].Host, s.plan.Nodes[2].Host}
		Expect(hosts).To(ConsistOf("node005", "node006"))
	})
})

var _ = Describe("srun: accepting steppers while they start", func() {
	It("takes one connection per host and closes a second one from the same host", func() {
		srv, err := proto.Listen("127.0.0.1:0", "tok")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(srv.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conns := map[string]*proto.Conn{}
		first, err := proto.Dial(srv.Addr(), "tok", "node001")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(first.Close)
		Expect(acceptSteppers(ctx, srv, 1, conns)).To(Succeed())

		// node001 is taken; another node001 must not count as the second host.
		dup, err := proto.Dial(srv.Addr(), "tok", "node001")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(dup.Close)
		second, err := proto.Dial(srv.Addr(), "tok", "node002")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(second.Close)
		Expect(acceptSteppers(ctx, srv, 2, conns)).To(Succeed())

		Expect(conns).To(HaveLen(2))
		Expect(conns).To(HaveKey("node001"))
		Expect(conns).To(HaveKey("node002"))
	})
})

// stepperLauncher starts a fake stepper for every Start, which follows the real
// protocol: dial srun, read its StepSpec, report every rank as exited 0. Start
// itself then blocks for delay, as qrsh's rejection window does.
type stepperLauncher struct {
	delay   time.Duration
	noDial  bool              // the stepper never connects
	dialAs  map[string]string // host -> the name its stepper claims
	onStart map[string]func() // called inside Start, before the delay
	closed  chan string       // hosts whose stepper lost its channel before a spec
}

func (l *stepperLauncher) Start(_ context.Context, host string, env proto.Envelope, token string) (launch.Handle, error) {
	if !l.noDial {
		go l.stepper(host, env, token)
	}
	if f := l.onStart[host]; f != nil {
		f()
	}
	time.Sleep(l.delay)
	return exitedHandle(host), nil
}

func (l *stepperLauncher) stepper(host string, env proto.Envelope, token string) {
	as := host
	if a := l.dialAs[host]; a != "" {
		as = a
	}
	c, err := proto.Dial(env.Dial, token, as)
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	f, err := c.Recv()
	if err != nil {
		l.closed <- host
		return
	}
	spec, err := proto.DecodeSpec(f.Payload)
	if err != nil {
		return
	}
	for _, r := range spec.Ranks {
		_ = c.Send(proto.Frame{Type: proto.FrameRankExit, Rank: uint32(r.Rank), Payload: proto.EncodeInt32(0)})
	}
}

var _ = Describe("srun: launch() as a whole", func() {
	var l *stepperLauncher
	var s *supervisor
	BeforeEach(func() {
		l = &stepperLauncher{closed: make(chan string, 8)}
		s = launchSupervisor(3)
		s.plan.NTasks = 3
		s.opt.command = []string{"true"}
		s.stdout = gbytes.NewBuffer()
		s.cfg.LaunchTimeout.Duration = 2 * time.Second
		origL, origC, origH := launchersFor, launchConcurrency, proto.HelloTimeout
		launchersFor = func(*supervisor) (launch.Launcher, launch.Launcher, error) { return l, l, nil }
		DeferCleanup(func() { launchersFor, launchConcurrency, proto.HelloTimeout = origL, origC, origH })
	})

	// The bug this fixes: steppers that connected early were dropped while
	// srun was still starting the others.
	It("keeps a stepper that connected while later nodes are still starting", func() {
		proto.HelloTimeout = 300 * time.Millisecond
		launchConcurrency = 1
		l.delay = 400 * time.Millisecond // 3 starts: 1.2s, far past the 300ms hold
		Expect(s.launch()).To(Equal(0), string(s.stderr.(*gbytes.Buffer).Contents()))
	})

	It("gives up launch_timeout after the last start when a stepper never connects", func() {
		l.noDial = true
		s.cfg.LaunchTimeout.Duration = 300 * time.Millisecond
		begin := time.Now()
		Expect(s.launch()).To(Equal(exitLauncher))
		Expect(time.Since(begin)).To(BeNumerically("<", 5*time.Second))
		Expect(string(s.stderr.(*gbytes.Buffer).Contents())).To(ContainSubstring("did not connect within launch_timeout"))
	})

	It("aborts with 128+signal on an interrupt during the starts and closes the steppers already accepted", func() {
		launchConcurrency = 1
		l.onStart = map[string]func(){"node002": func() { s.forward(syscall.SIGTERM) }}
		l.delay = 100 * time.Millisecond
		Expect(s.launch()).To(Equal(128 + int(syscall.SIGTERM)))
		Eventually(l.closed, "5s").Should(Receive(Equal("node001")), "the stepper exits by code, not by a signal")
	})

	It("refuses to push specs when a stray host filled a node's place", func() {
		l.dialAs = map[string]string{"node003": "node009"}
		Expect(s.launch()).To(Equal(exitLauncher))
		Expect(string(s.stderr.(*gbytes.Buffer).Contents())).To(ContainSubstring("no stepper connected for node003"))
	})
})
