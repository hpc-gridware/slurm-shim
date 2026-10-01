package srun

import (
	"context"
	"io"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"

	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// fakeHandle blocks Wait until released. launch.Handle has no Kill on purpose:
// srun must never kill a stepper's launcher handle (see killEscalation).
type fakeHandle struct {
	host    string
	release chan struct{}
}

func (h *fakeHandle) Host() string { return h.host }
func (h *fakeHandle) Wait() error  { <-h.release; return nil }

func exitedHandle(host string) *fakeHandle {
	h := &fakeHandle{host: host, release: make(chan struct{})}
	close(h.release)
	return h
}

// channelPair returns the listener plus srun's end and the stepper's end of one
// authenticated loopback control channel.
func channelPair() (srv *proto.Server, srunSide, stepperSide *proto.Conn, token string) {
	token, err := proto.NewToken()
	Expect(err).NotTo(HaveOccurred())
	srv, err = proto.Listen("127.0.0.1:0", token)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = srv.Close() })

	dialed := make(chan *proto.Conn, 1)
	go func() {
		defer GinkgoRecover()
		c, err := proto.Dial(srv.Addr(), token, "node002")
		Expect(err).NotTo(HaveOccurred())
		dialed <- c
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srunSide, err = srv.Accept(ctx)
	Expect(err).NotTo(HaveOccurred())
	stepperSide = <-dialed
	DeferCleanup(func() { _ = srunSide.Close(); _ = stepperSide.Close() })
	return srv, srunSide, stepperSide, token
}

// recvSignals collects the signal numbers the stepper side receives until its
// channel closes.
func recvSignals(c *proto.Conn) (<-chan int32, <-chan struct{}) {
	sigs := make(chan int32, 8)
	closed := make(chan struct{})
	go func() {
		for {
			f, err := c.Recv()
			if err != nil {
				close(closed)
				return
			}
			if f.Type == proto.FrameSig {
				sigs <- proto.DecodeInt32(f.Payload)
			}
		}
	}()
	return sigs, closed
}

var _ = Describe("srun kill-on-bad-exit escalation [REQ-STP-004]", func() {
	BeforeEach(func() {
		orig := killEscalation
		killEscalation = 150 * time.Millisecond
		DeferCleanup(func() { killEscalation = orig })
	})

	It("escalates SIGTERM, then SIGKILL, then abandons the channel, each a stage apart", func() {
		// A rank that refuses to die must not hang srun, and every stage goes over
		// the control channel: killing the stepper's launcher handle (the local
		// qrsh -inherit client) would delete the whole job (failed 100).
		_, srunSide, stepperSide, _ := channelPair()
		sigs, closed := recvSignals(stepperSide)
		s := &supervisor{stderr: io.Discard, kill: true, conns: []*proto.Conn{srunSide}}

		s.recordExit(5)

		Eventually(sigs, "1s").Should(Receive(Equal(int32(syscallSIGTERM))))
		Consistently(sigs, "75ms").ShouldNot(Receive(), "SIGKILL waits one killEscalation")
		Eventually(sigs, "1s").Should(Receive(Equal(int32(syscallSIGKILL))))
		Consistently(closed, "75ms").ShouldNot(BeClosed(), "abandon waits another killEscalation")
		Eventually(closed, "1s").Should(BeClosed())
	})

	It("arms the escalation only once, however many times it is triggered", func() {
		_, srunSide, stepperSide, _ := channelPair()
		sigs, _ := recvSignals(stepperSide)
		s := &supervisor{stderr: io.Discard, kill: true, conns: []*proto.Conn{srunSide}}

		s.recordExit(5)
		s.armEscalation() // e.g. srun also received SIGTERM

		Eventually(sigs, "1s").Should(Receive(Equal(int32(syscallSIGTERM))))
		Eventually(sigs, "1s").Should(Receive(Equal(int32(syscallSIGKILL))))
		Consistently(sigs, "400ms").ShouldNot(Receive(), "a second SIGKILL stage means it was armed twice")
	})

	It("does not escalate when kill-on-bad-exit is off", func() {
		_, srunSide, stepperSide, _ := channelPair()
		sigs, closed := recvSignals(stepperSide)
		s := &supervisor{stderr: io.Discard, kill: false, conns: []*proto.Conn{srunSide}}

		s.recordExit(5)

		Consistently(sigs, "400ms", "10ms").ShouldNot(Receive())
		Consistently(closed, "100ms", "10ms").ShouldNot(BeClosed())
	})
})

var _ = Describe("srun waits a bounded time for stepper processes", func() {
	It("returns as soon as the handles exit, without a warning", func() {
		stderr := gbytes.NewBuffer()
		s := &supervisor{stderr: stderr}

		s.waitHandles([]launch.Handle{exitedHandle("node001"), exitedHandle("node002")})

		Expect(string(stderr.Contents())).To(BeEmpty())
	})

	It("gives up on a stepper that never exits and names its host", func() {
		orig := stepperExitWait
		stepperExitWait = 30 * time.Millisecond
		DeferCleanup(func() { stepperExitWait = orig })

		stuck := &fakeHandle{host: "node002", release: make(chan struct{})}
		DeferCleanup(func() { close(stuck.release) })
		stderr := gbytes.NewBuffer()
		s := &supervisor{stderr: stderr}

		done := make(chan struct{})
		go func() { s.waitHandles([]launch.Handle{exitedHandle("node001"), stuck}); close(done) }()

		Eventually(done, "1s").Should(BeClosed())
		Expect(string(stderr.Contents())).To(ContainSubstring("stepper on node002 did not exit"))
		Expect(string(stderr.Contents())).NotTo(ContainSubstring("node001"))
	})
})

var _ = Describe("srun aborting a launch", func() {
	It("closes the listener and every channel, then waits for the steppers", func() {
		// A step that fails before supervision must leave no stepper waiting on
		// its channel: srun waits for them instead of exiting and leaving a qrsh
		// client that could die by SIGPIPE (and take the pe task with it).
		srv, srunSide, stepperSide, token := channelPair()
		_, closed := recvSignals(stepperSide)
		s := &supervisor{stderr: io.Discard}

		s.abortLaunch(srv, map[string]*proto.Conn{"node002": srunSide}, []launch.Handle{exitedHandle("node002")})

		Eventually(closed, "1s").Should(BeClosed(), "a connected stepper loses its channel")
		_, err := proto.Dial(srv.Addr(), token, "node003")
		Expect(err).To(HaveOccurred(), "a stepper still dialling is refused")
	})
})

var _ = Describe("srun signal forwarding [REQ-RUN-021]", func() {
	BeforeEach(func() {
		orig := killEscalation
		killEscalation = 150 * time.Millisecond
		DeferCleanup(func() { killEscalation = orig })
	})

	It("marks the launch interrupted on a terminating signal before the steppers connect", func() {
		// Nothing to forward to yet; srun must not die by the default action
		// mid-launch, so the launch aborts at its next step instead.
		s := &supervisor{stderr: io.Discard, interrupted: make(chan struct{})}

		s.forward(syscall.SIGUSR1) // a notification, not a termination: dropped
		Expect(s.interrupted).NotTo(BeClosed())
		Expect(s.launchInterrupted()).To(BeZero())

		s.forward(syscall.SIGTERM)
		Expect(s.interrupted).To(BeClosed())
		Expect(s.launchInterrupted()).To(Equal(syscall.SIGTERM))
	})

	It("forwards a signal to every stepper once connected, and bounds a SIGTERM", func() {
		_, srunSide, stepperSide, _ := channelPair()
		sigs, closed := recvSignals(stepperSide)
		s := &supervisor{stderr: io.Discard, conns: []*proto.Conn{srunSide}, connected: true}

		s.forward(syscall.SIGTERM)

		Eventually(sigs, "1s").Should(Receive(Equal(int32(syscall.SIGTERM))))
		Eventually(sigs, "1s").Should(Receive(Equal(int32(syscall.SIGKILL))), "a forwarded SIGTERM arms the escalation")
		Eventually(closed, "1s").Should(BeClosed())
	})

	It("forwards a single SIGINT without escalating", func() {
		_, srunSide, stepperSide, _ := channelPair()
		sigs, _ := recvSignals(stepperSide)
		s := &supervisor{stderr: io.Discard, conns: []*proto.Conn{srunSide}, connected: true}

		s.forward(syscall.SIGINT)

		Eventually(sigs, "1s").Should(Receive(Equal(int32(syscall.SIGINT))))
		Consistently(sigs, "400ms").ShouldNot(Receive(), "a single SIGINT keeps its two-press path")
	})
})
