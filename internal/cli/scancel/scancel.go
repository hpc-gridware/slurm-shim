// Package scancel implements the scancel shim (spec sec. 7.8): it maps job and
// array-task cancellation to qdel.
package scancel

import (
	"context"
	"fmt"
	"io"
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
		case a == "-s" || a == "--signal" || strings.HasPrefix(a, "--signal="):
			// Signalling a running step is deferred (SI-16). Warn and continue
			// with a plain qdel; consume the value of the space-separated form.
			fmt.Fprintln(stderr, "scancel: warning: --signal is not supported (slurm-shim); cancelling the job")
			if a == "-s" || a == "--signal" {
				i++ // skip its value
			}
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
		if code := qdel(runner, stderr, qdelArgs(id)...); code != 0 {
			worst = code
		}
	}
	return worst
}

// qdelArgs maps a SLURM id to qdel arguments. An array task "4711_2" cancels
// only that task via "qdel -t 2 4711" (SI-16).
func qdelArgs(id string) []string {
	if i := strings.IndexByte(id, '_'); i >= 0 {
		return []string{"-t", id[i+1:], id[:i]}
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
