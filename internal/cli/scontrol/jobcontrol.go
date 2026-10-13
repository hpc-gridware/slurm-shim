package scontrol

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// geTarget turns a SLURM job id into the GE job (or job.task) id the GE
// clients take: "4711" stays, "4711_2" becomes "4711.3" (SLURM index 2 is GE
// task 3, SI-14). ok is false for anything that is not such an id.
func geTarget(id string) (string, bool) {
	base, index, isTask := strings.Cut(id, "_")
	if n, err := strconv.Atoi(base); err != nil || n <= 0 {
		return "", false
	}
	if !isTask {
		return base, true
	}
	task, ok := gedata.GETask(index)
	if !ok {
		return "", false
	}
	return base + "." + task, true
}

// jobList splits scontrol's job list: ids separated by commas, spaces, or
// both, across one or more arguments ("1,2 3").
func jobList(args []string) []string {
	var ids []string
	for _, a := range args {
		ids = append(ids, strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ' ' })...)
	}
	return ids
}

// holdCmd places (qhold) or releases (qrls) a user hold on each job. SLURM's
// hold, uhold and release act on pending jobs; GE's user hold is the same
// thing, and a hold on a running job is accepted by GE and takes effect only
// if the job is requeued. Each id is its own call so one bad id does not stop
// the others, as in SLURM; the exit status is 1 when any failed.
func holdCmd(runner gedata.Runner, client string, args []string, stderr io.Writer) int {
	ids := jobList(args)
	if len(ids) == 0 {
		fmt.Fprintln(stderr, "scontrol: error: no job id specified")
		return 1
	}
	rc := 0
	for _, id := range ids {
		target, ok := geTarget(id)
		if !ok {
			fmt.Fprintf(stderr, "scontrol: error: Invalid job id specified: %s\n", id)
			rc = 1
			continue
		}
		out, errOut, exit, err := runner.Run(context.Background(), client, target)
		if err != nil {
			fmt.Fprintf(stderr, "scontrol: error: running %s: %v\n", client, err)
			rc = 1
			continue
		}
		if exit != 0 {
			fmt.Fprintf(stderr, "scontrol: error: %s: %s\n", id, geMessage(out, errOut, client+" failed"))
			rc = 1
		}
	}
	return rc
}

// suspendCmd suspends (qmod -sj) or resumes (qmod -usj) each job. qmod exits 0
// even when it refuses -- "Modify operation can not be applied on job-array
// task 1376.1 in pending/hold state", printed on stdout (OCS 9.1.6) -- so
// success is read from its answer: "suspended job N" / "unsuspended job N", or
// "is already (un)suspended", which SLURM scripts treat as done as well. In
// SLURM only an operator may suspend; GE lets the owner suspend their own job
// unless the site forbids it, and GE's refusal is passed through.
func suspendCmd(runner gedata.Runner, flag string, args []string, stderr io.Writer) int {
	ids := jobList(args)
	if len(ids) == 0 {
		fmt.Fprintln(stderr, "scontrol: error: no job id specified")
		return 1
	}
	rc := 0
	for _, id := range ids {
		target, ok := geTarget(id)
		if !ok {
			fmt.Fprintf(stderr, "scontrol: error: Invalid job id specified: %s\n", id)
			rc = 1
			continue
		}
		out, errOut, exit, err := runner.Run(context.Background(), "qmod", flag, target)
		if err != nil {
			fmt.Fprintf(stderr, "scontrol: error: running qmod: %v\n", err)
			rc = 1
			continue
		}
		if exit != 0 || !suspendAccepted(out) {
			fmt.Fprintf(stderr, "scontrol: error: %s: %s\n", id, geMessage(out, errOut, "qmod failed"))
			rc = 1
		}
	}
	return rc
}

// suspendAccepted reads qmod -sj/-usj's stdout. Under a dry run qmod never
// ran, so there is no answer to read.
func suspendAccepted(out []byte) bool {
	if dryrun.Enabled() {
		return true
	}
	s := string(out)
	return strings.Contains(s, "suspended job") || strings.Contains(s, "is already")
}

// geMessage is GE's own explanation of a failure: stderr, else stdout (qmod
// and qalter report some refusals there), else fallback.
func geMessage(out, errOut []byte, fallback string) string {
	if msg := strings.TrimSpace(string(errOut)); msg != "" {
		return msg
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return msg
	}
	return fallback
}
