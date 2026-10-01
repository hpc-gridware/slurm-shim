package srun

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// syscallSIGTERM is the signal number used for the kill-on-bad-exit fan-out.
const syscallSIGTERM = int(syscall.SIGTERM)

// syscallSIGKILL is the signal number of the kill-on-bad-exit escalation.
const syscallSIGKILL = int(syscall.SIGKILL)

// installSignals forwards the signals srun receives to every stepper over the
// control channel (REQ-RUN-021). A second SIGINT within one second escalates to
// a SIGKILL fan-out (SLURM-like).
//
// It is installed before the first stepper is launched. Dying by the Go default
// in the middle of a launch would leave qrsh clients writing their stderr into
// the pipe of an exited srun (a SIGPIPE there kills the client and, with it, the
// remote pe task), so a signal during the launch aborts it cleanly instead.
func (s *supervisor) installSignals() {
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP,
		syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGQUIT)
	go func() {
		var lastInt time.Time
		for sig := range ch {
			ssig, ok := sig.(syscall.Signal)
			if !ok {
				continue
			}
			if ssig == syscall.SIGINT && time.Since(lastInt) < time.Second {
				s.forward(syscall.SIGKILL)
				continue
			}
			if ssig == syscall.SIGINT {
				lastInt = time.Now()
			}
			s.forward(ssig)
		}
	}()
}

// forward delivers a signal srun received. Once the steppers are connected it
// goes to every one of them; SIGTERM/SIGHUP end the step (scancel, timeout, a
// dropped session) and are bounded like kill-on-bad-exit, so a rank that ignores
// them cannot hang srun. A single SIGINT keeps its own two-press path.
//
// During the launch there is no one to forward to: the first terminating signal
// marks the launch interrupted (launch aborts at its next step), and a second
// one exits at once, as the user insists.
func (s *supervisor) forward(sig syscall.Signal) {
	s.mu.Lock()
	if !s.connected {
		if !isTerminating(sig) {
			s.mu.Unlock()
			return
		}
		if s.interruptSig != 0 {
			s.mu.Unlock()
			os.Exit(128 + int(sig))
		}
		s.interruptSig = sig
		close(s.interrupted)
		s.mu.Unlock()
		return
	}
	conns := s.conns
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Send(proto.Frame{Type: proto.FrameSig, Payload: proto.EncodeInt32(int32(sig))})
	}
	if sig == syscall.SIGTERM || sig == syscall.SIGHUP {
		s.armEscalation()
	}
}

// launchInterrupted reports the signal that interrupted the launch, or 0.
func (s *supervisor) launchInterrupted() syscall.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interruptSig
}

func isTerminating(sig syscall.Signal) bool {
	switch sig {
	case syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGKILL:
		return true
	}
	return false
}
