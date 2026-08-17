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

// installSignals forwards the signals srun receives to every stepper over the
// control channel (REQ-RUN-021). A second SIGINT within one second escalates to
// a SIGKILL fan-out (SLURM-like).
func (s *supervisor) installSignals(conns map[string]*proto.Conn) {
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
				s.sendSig(conns, int(syscall.SIGKILL))
				continue
			}
			if ssig == syscall.SIGINT {
				lastInt = time.Now()
			}
			s.sendSig(conns, int(ssig))
		}
	}()
}

func (s *supervisor) sendSig(conns map[string]*proto.Conn, signo int) {
	for _, c := range conns {
		_ = c.Send(proto.Frame{Type: proto.FrameSig, Payload: proto.EncodeInt32(int32(signo))})
	}
}
