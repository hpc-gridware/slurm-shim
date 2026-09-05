package launch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// SessionSpec is an interactive allocation request: what `srun --pty` outside
// an allocation asks for, already translated to Grid Engine terms.
type SessionSpec struct {
	Queue string
	PE    string
	Slots int
	// AllocationRule is the -par value pinning the layout, or "" for none.
	AllocationRule string
	// VerifyGeometry adds -w e alongside -par. False when the request names a
	// load-sensor memory complex, which -w e cannot see and would refuse
	// (submit.VerifyGeometry decides).
	VerifyGeometry bool
	// Resources is the -l list (h_rt, memory complex, GPU complex), or "".
	Resources string
	Account   string
	JobName   string
	// Chdir is --chdir; "" means -cwd, SLURM's default of the invocation dir.
	Chdir string
	// Export is the -V / -v argument list from --export.
	Export []string
	// Command is the user's command and its arguments, passed through verbatim.
	Command []string
}

// SessionArgs builds the qrsh argv for an interactive session. It is the single
// builder: ExecSession runs it and the srun dry run prints it, so the reported
// command line cannot drift from the one Grid Engine receives.
//
// Every fixed flag here was verified live on OCS 9.1.5:
//
//   - -now no: qrsh's default is an immediate job, rejected if it cannot start at
//     once and confined to INTERACTIVE-type queues; -now no queues and waits, as
//     srun does, and may use batch queues.
//   - -pty y: a qrsh with a command gets no pty by default; -pty y forces one, so
//     the session lands on a real terminal.
//   - -cwd: without it the session starts in $HOME; SLURM starts in the
//     invocation directory. -wd overrides it when --chdir was given.
//
// The command is appended last with no terminator (Grid Engine has no "--"), so
// a user command beginning with "-" would be read by qrsh as its own option --
// the caller runs as the user and can only affect its own job, but srun should
// reject such a command rather than mis-submit it.
func SessionArgs(s SessionSpec) []string {
	args := []string{"-now", "no", "-pty", "y"}
	if s.Chdir != "" {
		args = append(args, "-wd", s.Chdir)
	} else {
		args = append(args, "-cwd")
	}
	args = append(args, "-q", s.Queue, "-pe", s.PE, strconv.Itoa(s.Slots))
	if s.AllocationRule != "" {
		// -w e rides along with -par, never alone: it turns an unsatisfiable
		// layout into a submit-time refusal instead of a job wedged in qw. On qrsh
		// the refusal is "error: no suitable queues" (verified), NOT a rejection to
		// retry -- do not feed it to the stepper's classifyRejection.
		args = append(args, "-par", s.AllocationRule)
		if s.VerifyGeometry {
			args = append(args, "-w", "e")
		}
	}
	if s.Resources != "" {
		args = append(args, "-l", s.Resources)
	}
	if s.Account != "" {
		// -A is a free-text accounting string that never fails and round-trips to
		// SLURM_JOB_ACCOUNT via SGE_ACCOUNT (verified). It is NOT -P (a project,
		// which must exist and does not set SGE_ACCOUNT).
		args = append(args, "-A", s.Account)
	}
	if s.JobName != "" {
		args = append(args, "-N", s.JobName)
	}
	args = append(args, s.Export...)
	args = append(args, s.Command...)
	return args
}

// ExecSession replaces the shim process with an interactive `qrsh`, so qrsh owns
// the terminal, signals, the allocation wait, teardown and the exit status
// directly -- verified: bash exit 3 surfaces as 3, Ctrl-C while queued deletes
// the pending job, an h_rt kill restores the terminal. This never returns on
// success; it returns an error only if the exec itself fails.
//
// QRSH_WRAPPER / SGE_RSH_COMMAND are scrubbed from the environment (qrshEnv):
// verified that a caller-set QRSH_WRAPPER otherwise replaces the command outright
// on this path. os/exec / syscall lives here because REQ-IMP-001 confines
// external process execution to internal/launch.
func ExecSession(s SessionSpec) error {
	// syscall.Exec does no PATH lookup, so an absolute path is required.
	// ResolveCommand returns a bare name when qrsh is on PATH (it is built for
	// exec.Command, which resolves PATH itself), so resolve it the rest of the way.
	qrsh := gedata.ResolveCommand("qrsh")
	if !filepath.IsAbs(qrsh) {
		abs, err := exec.LookPath(qrsh)
		if err != nil {
			return fmt.Errorf("locating qrsh: %w", err)
		}
		qrsh = abs
	}
	argv := append([]string{qrsh}, SessionArgs(s)...)
	env := qrshEnv(os.Environ())
	if err := syscall.Exec(qrsh, argv, env); err != nil {
		return fmt.Errorf("exec %s: %w", qrsh, err)
	}
	return nil // unreachable on success: the process image is replaced
}
