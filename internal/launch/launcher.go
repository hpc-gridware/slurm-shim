// Package launch starts one stepper on one host. The control connection back to
// srun is managed by the proto.Server (the stepper dials in), so a Launcher only
// needs to spawn the stepper process; the same interface backs the local
// launcher (M3, and the fake for tests) and the qrsh launcher (M4). D-6.
package launch

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// Launcher spawns a stepper for one host.
type Launcher interface {
	// Start launches a stepper carrying envelope in its argv and token in its
	// environment (never argv, SI-35). The stepper dials the control address in
	// the envelope; Start returns once the process is running.
	Start(ctx context.Context, host string, envelope proto.Envelope, token string) (Handle, error)
}

// ErrHostUnusable marks a start failure that points at the host itself (its
// execd never accepted the task), as opposed to one that would fail the same way
// on any host (spawning qrsh, slots held by other steps, a misconfiguration).
// Only the former may cost a hot spare: errors.Is(err, ErrHostUnusable).
var ErrHostUnusable = errors.New("host unusable")

// HostError marks err as pointing at the host (ErrHostUnusable), keeping its
// message.
func HostError(err error) error { return hostError{err} }

type hostError struct{ error }

func (e hostError) Unwrap() error        { return e.error }
func (e hostError) Is(target error) bool { return target == ErrHostUnusable }

// Handle is a launched stepper's lifecycle. It deliberately has no Kill: under
// tight integration the handle is the local `qrsh -inherit` client, and killing
// it makes the remote pe task die by signal, which deletes the whole job. Steps
// are stopped over the control channel instead.
type Handle interface {
	Host() string
	Wait() error
}

// LocalLauncher spawns the stepper as a local subprocess (the master host's own
// ranks, REQ-RUN-012, and the single-machine test path that exercises the real
// control channel over loopback, D-6).
type LocalLauncher struct {
	// Self is the shim binary path (os.Executable()).
	Self string
	// Stderr receives the stepper's own diagnostics; rank output is framed over
	// the channel, not written here.
	Stderr io.Writer
}

// Start spawns `<self> stepper --envelope <token>` with SLURM_SHIM_TOKEN set.
func (l LocalLauncher) Start(_ context.Context, host string, env proto.Envelope, token string) (Handle, error) {
	arg, err := proto.EncodeEnvelope(env)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(l.Self, "stepper", "--envelope", arg)
	cmd.Env = append(os.Environ(), "SLURM_SHIM_TOKEN="+token)
	cmd.Stderr = l.Stderr
	// Own process group, like the qrsh client: a terminal Ctrl-C reaches srun
	// only, and srun forwards it to every stepper alike. In srun's group the
	// local stepper would take a single SIGINT as its own termination (TERM, then
	// KILL) and end the master's ranks, where SLURM just forwards the SIGINT.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &localHandle{host: host, cmd: cmd}, nil
}

type localHandle struct {
	host string
	cmd  *exec.Cmd
}

func (h *localHandle) Host() string { return h.host }
func (h *localHandle) Wait() error  { return h.cmd.Wait() }
