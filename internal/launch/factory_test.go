package launch

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

var _ = Describe("launcher factory [REQ-RUN-012, REQ-RUN-013]", func() {
	It("selects the qrsh launcher for slave hosts by default", func() {
		l, err := For(&config.Config{Launcher: "qrsh-inherit"}, "/shim", io.Discard)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).To(BeAssignableToTypeOf(QrshLauncher{}))
	})

	It("selects the local launcher in local mode", func() {
		l, err := For(&config.Config{Launcher: "local"}, "/shim", io.Discard)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).To(BeAssignableToTypeOf(LocalLauncher{}))
	})

	It("defaults an empty launcher to qrsh", func() {
		l, err := For(&config.Config{Launcher: ""}, "/shim", io.Discard)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).To(BeAssignableToTypeOf(QrshLauncher{}))
	})

	It("rejects the unimplemented ssh backend", func() {
		_, err := For(&config.Config{Launcher: "ssh"}, "/shim", io.Discard)
		Expect(err).To(MatchError(ContainSubstring("not implemented")))
	})

	It("rejects an unknown launcher", func() {
		_, err := For(&config.Config{Launcher: "bogus"}, "/shim", io.Discard)
		Expect(err).To(MatchError(ContainSubstring("unknown launcher")))
	})
})

var _ = Describe("execProc real process lifecycle", func() {
	// newExecProc mirrors spawnQrsh for an arbitrary command so the real
	// settle/Wait/tailBuffer paths are exercised without a qrsh binary.
	newExecProc := func(name string, args ...string) *execProc {
		cmd := exec.Command(name, args...)
		buf := &tailBuffer{limit: 8192}
		cmd.Stderr = buf
		Expect(cmd.Start()).To(Succeed())
		return &execProc{cmd: cmd, stderr: buf, done: waitCh(cmd)}
	}

	It("reports an early exit with the captured stderr", func() {
		p := newExecProc("sh", "-c", "echo boom >&2; exit 4")
		exited, tail := p.settle(2 * time.Second)
		Expect(exited).To(BeTrue())
		Expect(tail).To(ContainSubstring("boom"))
	})

	It("does not report exit while the process is still running", func() {
		p := newExecProc("sh", "-c", "sleep 1; exit 0")
		exited, _ := p.settle(50 * time.Millisecond)
		Expect(exited).To(BeFalse())
		Expect(p.Wait()).To(Succeed())
	})

	It("Wait returns the process exit error", func() {
		p := newExecProc("sh", "-c", "exit 7")
		p.settle(time.Second)
		err := p.Wait()
		Expect(err).To(HaveOccurred())
		var ee *exec.ExitError
		Expect(errors.As(err, &ee)).To(BeTrue())
		Expect(ee.ExitCode()).To(Equal(7))
	})
})

var _ = Describe("QrshLauncher context cancellation", func() {
	It("aborts the retry loop when the context is cancelled", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		l := QrshLauncher{Self: "/shim", Stderr: io.Discard}
		env := proto.Envelope{JobID: 4711, StepID: 1, Host: "node002", NodeID: 1, Dial: "127.0.0.1:5000"}
		_, err := l.Start(ctx, "node002", env, "tok")
		Expect(err).To(MatchError(ContainSubstring("cancelled")))
	})
})

var _ = Describe("local stepper process group", func() {
	It("starts the master's stepper in its own process group", func() {
		// A terminal Ctrl-C must reach srun only, which forwards it to every
		// stepper alike; in srun's group the local stepper would end its ranks.
		self := filepath.Join(GinkgoT().TempDir(), "fake-shim")
		Expect(os.WriteFile(self, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700)).To(Succeed())

		h, err := LocalLauncher{Self: self, Stderr: io.Discard}.Start(context.Background(), "node001", proto.Envelope{}, "token")
		Expect(err).NotTo(HaveOccurred())
		lh := h.(*localHandle)
		DeferCleanup(func() { _ = lh.cmd.Process.Kill(); _ = h.Wait() })

		pid := lh.cmd.Process.Pid
		pgid, err := syscall.Getpgid(pid)
		Expect(err).NotTo(HaveOccurred())
		Expect(pgid).To(Equal(pid))
		Expect(pgid).NotTo(Equal(syscall.Getpgrp()))
	})
})
