/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

package launch

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// SessionSpec is an allocation that runs one command: what `srun` and `salloc`
// outside an allocation ask for, already translated to Grid Engine terms.
type SessionSpec struct {
	// Pty gives the command a terminal (srun --pty, salloc at a terminal).
	Pty bool
	// Immediate is srun/salloc -I: start at once or fail, never queue.
	Immediate bool
	Queue     string
	PE        string
	Slots     int
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
//     srun does, and may use batch queues. -I asks for exactly that immediate
//     job (-now yes).
//   - -pty y: a qrsh with a command gets no pty by default; -pty y forces one, so
//     the session lands on a real terminal. Without Pty no -pty flag is passed
//     at all: an explicit -pty no failed every job in the shepherd ("fd for in
//     is not 0") and set the queue instance to error state (OCS 9.1.6), while
//     the default streams stdin, stdout, stderr and the exit status.
//   - -cwd: without it the session starts in $HOME; SLURM starts in the
//     invocation directory. -wd overrides it when --chdir was given.
//
// The command is appended last with no terminator (Grid Engine has no "--"), so
// a user command beginning with "-" would be read by qrsh as its own option --
// the caller runs as the user and can only affect its own job, but srun should
// reject such a command rather than mis-submit it.
func SessionArgs(s SessionSpec) []string {
	now := "no"
	if s.Immediate {
		now = "yes"
	}
	args := []string{"-now", now}
	if s.Pty {
		args = append(args, "-pty", "y")
	}
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
	qrsh, err := qrshPath()
	if err != nil {
		return err
	}
	argv := append([]string{qrsh}, SessionArgs(s)...)
	env := qrshEnv(os.Environ(), "")
	if err := syscall.Exec(qrsh, argv, env); err != nil {
		return fmt.Errorf("exec %s: %w", qrsh, err)
	}
	return nil // unreachable on success: the process image is replaced
}

// RunSession runs a session without a terminal as a child qrsh and returns its
// exit status. Unlike ExecSession it stays in front of qrsh, because qrsh
// itself does not react to the signals a caller ends a command with (verified,
// OCS 9.1.6): waiting for its job it ignores SIGTERM and SIGHUP and stays
// queued, so a killed caller would leave a job that starts later with no one
// to talk to. Only SIGINT ends it in every state -- a queued job is deleted, a
// running command is stopped and its job removed. So SIGINT, SIGTERM and
// SIGHUP received here are passed on as SIGINT, and the session then exits
// 128+signal, as a process killed by that signal would (qrsh itself exits 0
// or 1 there, which reads as success or a plain error).
//
// nullStdin gives qrsh /dev/null for input instead of the caller's stdin. qrsh's
// own -nostdin is not used: the remote command then waits for input that never
// comes (verified with cat), and qrsh ignores SIGTERM meanwhile.
func RunSession(s SessionSpec, nullStdin bool) (int, error) {
	qrsh, err := qrshPath()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(qrsh, SessionArgs(s)...)
	cmd.Env = qrshEnv(os.Environ(), "")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if !nullStdin {
		cmd.Stdin = os.Stdin
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	return supervise(cmd, sigs)
}

// supervise starts cmd, passes every signal from sigs on to it as SIGINT, and
// returns its exit status -- 128+signal for the first signal received.
func supervise(cmd *exec.Cmd, sigs <-chan os.Signal) (int, error) {
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting %s: %w", cmd.Path, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var got syscall.Signal
	for {
		select {
		case sig := <-sigs:
			if got == 0 {
				got = sig.(syscall.Signal)
			}
			_ = cmd.Process.Signal(syscall.SIGINT)
		case err := <-done:
			if got != 0 {
				return 128 + int(got), nil
			}
			return exitStatus(cmd, err)
		}
	}
}

// exitStatus is a finished command's exit code, 128+signal when it was killed.
func exitStatus(cmd *exec.Cmd, err error) (int, error) {
	if cmd.ProcessState == nil {
		return 0, err
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return cmd.ProcessState.ExitCode(), nil
}

// qrshPath resolves qrsh to an absolute path.
func qrshPath() (string, error) {
	qrsh := gedata.ResolveCommand("qrsh")
	if filepath.IsAbs(qrsh) {
		return qrsh, nil
	}
	abs, err := exec.LookPath(qrsh)
	if err != nil {
		return "", fmt.Errorf("locating qrsh: %w", err)
	}
	return abs, nil
}
