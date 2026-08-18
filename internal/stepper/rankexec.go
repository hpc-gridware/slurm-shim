package stepper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// rankFailExit is the exit code the trampoline uses for a pre-exec failure. The
// stepper never confuses it with a user exit because a failure also arrives on
// the status pipe (SI-21); the code only matters when the pipe is absent.
const rankFailExit = 127

// RankExec is the per-rank trampoline (D-3). The stepper spawns it as
// `<self> rank-exec --cpuset <m> --chdir <d> -- <cmd>...` with the rank's
// environment set on the child and a status pipe on fd 3. It locks the OS
// thread (so affinity and exec share one thread), applies the CPU mask (best-
// effort: a warning, not a failure, if it cannot be set), sets
// SLURM_TASK_PID to its own PID (execve preserves it, B04), chdirs, and execs
// the command. Any pre-exec failure is written to the status pipe so the stepper
// can emit a RANK_FAIL frame.
func RankExec(args []string) int {
	runtime.LockOSThread()

	status := os.NewFile(3, "status-pipe")
	fail := func(reason string) int {
		if status != nil {
			_, _ = io.WriteString(status, reason)
			_ = status.Close()
		}
		return rankFailExit
	}

	cpuset, chdir, cmd, err := parseRankExecArgs(args)
	if err != nil {
		return fail("rank-exec: " + err.Error())
	}

	// Close the status pipe on a successful exec so the stepper observes EOF and
	// knows the rank started; on failure the write above still reaches it first.
	if status != nil {
		unix.CloseOnExec(int(status.Fd()))
	}

	if cpuset != "" {
		if err := setAffinity(cpuset); err != nil {
			// Best-effort binding: a granted cpuset that cannot be applied (e.g. it
			// references CPUs absent in this environment, or affinity is restricted)
			// must not kill the rank. Warn and run unpinned, like SLURM does.
			fmt.Fprintf(os.Stderr, "slurm-shim: warning: could not set CPU affinity to %q: %v\n", cpuset, err)
		}
	}
	if chdir != "" {
		if err := os.Chdir(chdir); err != nil {
			return fail("chdir: " + err.Error())
		}
	}

	path, err := exec.LookPath(cmd[0])
	if err != nil {
		return fail("exec: " + err.Error())
	}
	env := append(os.Environ(), "SLURM_TASK_PID="+strconv.Itoa(os.Getpid()))
	if err := syscall.Exec(path, cmd, env); err != nil {
		return fail("exec: " + err.Error())
	}
	return 0 // unreachable: a successful Exec never returns
}

// parseRankExecArgs parses the trampoline argv up to the "--" command separator.
func parseRankExecArgs(args []string) (cpuset, chdir string, cmd []string, err error) {
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--cpuset":
			if i++; i >= len(args) {
				return "", "", nil, errors.New("--cpuset needs a value")
			}
			cpuset = args[i]
		case "--chdir":
			if i++; i >= len(args) {
				return "", "", nil, errors.New("--chdir needs a value")
			}
			chdir = args[i]
		case "--":
			cmd = args[i+1:]
			if len(cmd) == 0 {
				return "", "", nil, errors.New("no command after --")
			}
			return cpuset, chdir, cmd, nil
		default:
			return "", "", nil, fmt.Errorf("unknown rank-exec argument %q", args[i])
		}
		i++
	}
	return "", "", nil, errors.New("missing -- command separator")
}
