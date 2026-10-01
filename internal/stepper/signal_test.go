package stepper

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// The stepper is a Grid Engine pe task under tight integration. A pe task that
// dies BY a signal makes qmaster delete the whole job (failed 100), so every
// catchable terminating signal must end the stepper with an exit code instead.
// These specs run the real binary (shimBin, built by the suite): gexec reports
// death by signal as exit -1, so Exit(143) proves the stepper exited with a code.

// dialStepper opens a control channel and returns its listener, token and the
// envelope a stepper needs to dial it.
func dialStepper() (*proto.Server, string, string) {
	token, err := proto.NewToken()
	Expect(err).NotTo(HaveOccurred())
	srv, err := proto.Listen("127.0.0.1:0", token)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = srv.Close() })
	env, err := proto.EncodeEnvelope(proto.Envelope{JobID: 1, Host: "node001", Dial: srv.Addr()})
	Expect(err).NotTo(HaveOccurred())
	return srv, token, env
}

func acceptStepper(srv *proto.Server) *proto.Conn {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := srv.Accept(ctx)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = conn.Close() })
	return conn
}

// startStepper launches `slurm-shim stepper` dialling a fresh control channel
// and returns the session plus srun's end of the channel.
func startStepper() (*gexec.Session, *proto.Conn) {
	srv, token, env := dialStepper()
	cmd := exec.Command(shimBin, "stepper", "--envelope", env)
	cmd.Env = []string{"SLURM_SHIM_TOKEN=" + token, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	sess, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { sess.Kill() })
	return sess, acceptStepper(srv)
}

// frames delivers every frame srun's end receives, so a spec can wait with a
// timeout instead of blocking in Recv.
func frames(conn *proto.Conn) <-chan proto.Frame {
	ch := make(chan proto.Frame, 16)
	go func() {
		defer close(ch)
		for {
			f, err := conn.Recv()
			if err != nil {
				return
			}
			ch <- f
		}
	}()
	return ch
}

// runRank sends a one-rank StepSpec whose script prints "started <pid>" and
// returns that pid once it arrives, so a signal lands while the rank runs.
func runRank(conn *proto.Conn, script string) (int, <-chan proto.Frame) {
	payload, err := proto.EncodeSpec(proto.StepSpec{
		Env:     []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"},
		Command: []string{"sh", "-c", script},
		Ranks:   []proto.RankSpec{{Rank: 0}},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(conn.Send(proto.Frame{Type: proto.FrameSpec, Payload: payload})).To(Succeed())
	fs := frames(conn)
	var pid int
	Eventually(func() int {
		select {
		case f := <-fs:
			if f.Type == proto.FrameOut && strings.HasPrefix(string(f.Payload), "started ") {
				pid, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(f.Payload), "started ")))
			}
		default:
		}
		return pid
	}, "10s", "10ms").ShouldNot(BeZero())
	return pid, fs
}

func rankGone(pid int) func() error {
	return func() error { return syscall.Kill(pid, 0) }
}

var _ = Describe("stepper terminating signals [REQ-STP-004]", func() {
	DescribeTable("stops its ranks and exits 143 instead of dying by the signal",
		func(sig syscall.Signal) {
			sess, conn := startStepper()
			pid, fs := runRank(conn, `echo started $$; exec sleep 60`)

			Expect(sess.Command.Process.Signal(sig)).To(Succeed())

			Eventually(sess, "10s").Should(gexec.Exit(143))
			Eventually(rankGone(pid), "5s").Should(MatchError(syscall.ESRCH), "the rank must be stopped, not orphaned")
			Eventually(fs, "5s").Should(Receive(HaveField("Type", proto.FrameRankExit)), "the rank's exit is reported to srun")
		},
		Entry("SIGTERM", syscall.SIGTERM),
		Entry("SIGHUP", syscall.SIGHUP),
		Entry("SIGINT", syscall.SIGINT),
	)

	It("escalates to SIGKILL for a rank that ignores SIGTERM, and still exits 143", func() {
		sess, conn := startStepper()
		pid, _ := runRank(conn, `trap "" TERM; echo started $$; exec sleep 60`)

		Expect(sess.Command.Process.Signal(syscall.SIGTERM)).To(Succeed())

		// killWait (5s) separates SIGTERM and SIGKILL.
		Consistently(sess, "3s").ShouldNot(gexec.Exit())
		Eventually(sess, "10s").Should(gexec.Exit(143))
		Eventually(rankGone(pid), "5s").Should(MatchError(syscall.ESRCH))
	})

	It("exits 143 when signalled before any rank was started", func() {
		sess, _ := startStepper() // connected, still waiting for its StepSpec

		Expect(sess.Command.Process.Signal(syscall.SIGTERM)).To(Succeed())

		Eventually(sess, "5s").Should(gexec.Exit(143))
	})

	It("exits with a code, not by SIGPIPE, when its stderr is broken", func() {
		// srun (or the qrsh client relaying stderr) may be gone; a write to the
		// broken stderr must fail with EPIPE instead of killing the pe task.
		srv, token, env := dialStepper()
		r, w, err := os.Pipe()
		Expect(err).NotTo(HaveOccurred())
		cmd := exec.Command(shimBin, "stepper", "--envelope", env)
		cmd.Env = []string{"SLURM_SHIM_TOKEN=" + token, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
		cmd.Stderr = w
		Expect(cmd.Start()).To(Succeed())
		_ = w.Close()
		_ = r.Close() // no reader left: the stepper's next stderr write gets EPIPE
		conn := acceptStepper(srv)

		_ = conn.Close() // "no StepSpec received" goes to the broken stderr

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var waitErr error
		Eventually(done, "10s").Should(Receive(&waitErr))
		var ee *exec.ExitError
		Expect(errors.As(waitErr, &ee)).To(BeTrue())
		ws := ee.Sys().(syscall.WaitStatus)
		Expect(ws.Signaled()).To(BeFalse(), "died by %v", ws.Signal())
		Expect(ws.ExitStatus()).To(Equal(1))
	})
})

var _ = Describe("stepper kill latch", func() {
	It("never downgrades a latched SIGKILL to SIGTERM", func() {
		// Overlapping terminations (a signal plus srun's channel dropping, or a
		// SIGTERM forwarded after srun's SIGKILL stage) must not let a rank that
		// registers late get away with SIGTERM only.
		s := &stepper{}
		s.forwardSignal(syscall.SIGKILL)
		s.forwardSignal(syscall.SIGTERM)
		Expect(s.killSig).To(Equal(syscall.SIGKILL))
	})

	It("latches SIGTERM when nothing is latched yet", func() {
		s := &stepper{}
		s.forwardSignal(syscall.SIGTERM)
		Expect(s.killSig).To(Equal(syscall.SIGTERM))
	})
})
