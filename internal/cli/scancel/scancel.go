// Package scancel implements the scancel shim (spec sec. 7.8): it maps job and
// array-task cancellation to qdel. `scancel --signal` (submitit's
// checkpoint-preempt) maps instead to a GE reschedule (qmod -rj), which delivers
// SIGUSR2 to a -notify job and restarts it, so the job checkpoints and resumes.
package scancel

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// Run is the scancel entry point.
func Run(args []string, stdout, stderr io.Writer) int {
	return run(gedata.ExecRunner{}, args, stdout, stderr)
}

func run(runner gedata.Runner, args []string, stdout, stderr io.Writer) int {
	var jobIDs []string
	user := ""
	signal := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--user":
			if i++; i < len(args) {
				user = args[i]
			}
		case strings.HasPrefix(a, "--user="):
			user = a[len("--user="):]
		case strings.HasPrefix(a, "-u") && len(a) > 2:
			user = a[2:]
		case a == "-s" || a == "--signal":
			// A signal request is submitit's checkpoint-preempt, not a cancel:
			// reschedule instead (see below). Consume the space-form value.
			signal = true
			i++
		case strings.HasPrefix(a, "--signal="):
			signal = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(stderr, "scancel: warning: option %s ignored (slurm-shim)\n", a)
		default:
			jobIDs = append(jobIDs, a)
		}
	}

	if user != "" {
		return qdel(runner, stderr, "-u", user)
	}
	if len(jobIDs) == 0 {
		fmt.Fprintln(stderr, "scancel: error: no job id given")
		return 1
	}

	worst := 0
	for _, id := range jobIDs {
		var code int
		if signal {
			code = reschedule(runner, stderr, id)
		} else {
			code = qdel(runner, stderr, qdelArgs(id)...)
		}
		if code != 0 {
			worst = code
		}
	}
	return worst
}

// reschedule maps `scancel --signal` to `qmod -rj`, GE's reschedule. For a job
// submitted with -notify -r y (which the shim does when sbatch sees --signal),
// this delivers SIGUSR2 to the running job -- submitit's default preempt signal
// -- then terminates and restarts it, so submitit's handler checkpoints and the
// restart resumes from the checkpoint. It is best-effort: submitit's _interrupt
// issues two signals in a row, so a second reschedule of an already-rescheduling
// or finished job is warned, not failed.
func reschedule(runner gedata.Runner, stderr io.Writer, id string) int {
	target := id
	if i := strings.IndexByte(id, '_'); i >= 0 { // array element N_k -> N.<k+1>
		base, task := id[:i], id[i+1:]
		if n, err := strconv.Atoi(task); err == nil {
			target = base + "." + strconv.Itoa(n+1)
		}
	}
	_, errOut, exit, err := runner.Run(context.Background(), "qmod", "-rj", target)
	if err != nil {
		fmt.Fprintf(stderr, "scancel: error: running qmod: %v\n", err)
		return 1
	}
	if exit != 0 {
		if msg := strings.TrimSpace(string(errOut)); msg != "" {
			fmt.Fprintf(stderr, "scancel: warning: %s\n", msg)
		}
	}
	return 0
}

// qdelArgs maps a SLURM id to qdel arguments. A whole job "4711" -> "qdel 4711".
// An array element "4711_2" cancels one task via "qdel -t <ge> 4711": SLURM /
// submitit array indices are 0-based while GE tasks are 1-based, and the shim's
// sbatch/sacct use that same convention, so element k maps to GE task k+1 (this
// makes `scancel N_0` -- which submitit issues -- cancel the first element, and
// keeps scancel consistent with the ids sacct reports). Native 1-based GE arrays
// are therefore off by one through scancel; cancel those by the whole array or
// with qdel directly (SI-16).
func qdelArgs(id string) []string {
	if i := strings.IndexByte(id, '_'); i >= 0 {
		base, task := id[:i], id[i+1:]
		// GE requires the job id before -t (`qdel <id> -t <task>`); a leading -t is
		// rejected ("found lonely '-t' option").
		if n, err := strconv.Atoi(task); err == nil {
			return []string{base, "-t", strconv.Itoa(n + 1)}
		}
		return []string{base, "-t", task}
	}
	return []string{id}
}

func qdel(runner gedata.Runner, stderr io.Writer, args ...string) int {
	_, errOut, exit, err := runner.Run(context.Background(), "qdel", args...)
	if err != nil {
		fmt.Fprintf(stderr, "scancel: error: running qdel: %v\n", err)
		return 1
	}
	if exit != 0 {
		if msg := strings.TrimSpace(string(errOut)); msg != "" {
			fmt.Fprintf(stderr, "scancel: error: %s\n", msg)
		}
		return exit
	}
	return 0
}
