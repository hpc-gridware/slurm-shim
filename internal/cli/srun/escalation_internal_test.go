package srun

import (
	"io"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/launch"
)

// fakeHandle records whether Kill was called (the kill-escalation target).
type fakeHandle struct{ killed atomic.Bool }

func (h *fakeHandle) Host() string { return "h" }
func (h *fakeHandle) Wait() error  { return nil }
func (h *fakeHandle) Kill() error  { h.killed.Store(true); return nil }

var _ = Describe("srun kill-on-bad-exit escalation [REQ-STP-004]", func() {
	It("force-kills the stepper handles when the fan-out does not drain in time", func() {
		// A rank that refuses to die must not hang srun: after killEscalation the
		// handles are force-killed so supervise and h.Wait can return.
		orig := killEscalation
		killEscalation = 20 * time.Millisecond
		DeferCleanup(func() { killEscalation = orig })

		h := &fakeHandle{}
		s := &supervisor{stderr: io.Discard, kill: true, handles: []launch.Handle{h}}

		// A bad exit triggers the kill fan-out and schedules the watchdog.
		s.recordExit(5)

		Eventually(h.killed.Load, "1s", "5ms").Should(BeTrue(), "handle should be force-killed after killEscalation")
	})

	It("does not force-kill when kill-on-bad-exit is off", func() {
		orig := killEscalation
		killEscalation = 20 * time.Millisecond
		DeferCleanup(func() { killEscalation = orig })

		h := &fakeHandle{}
		s := &supervisor{stderr: io.Discard, kill: false, handles: []launch.Handle{h}}
		s.recordExit(5)

		Consistently(h.killed.Load, "100ms", "10ms").Should(BeFalse())
	})
})
