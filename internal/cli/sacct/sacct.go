// Package sacct implements a minimal sacct shim tailored to submitit's job
// tracking (submitit support plan, Phase 1). submitit is the only consumer that
// needs sacct in this shim; interactive users use squeue / scontrol. submitit
// runs
//
//	sacct -o JobID,State,NodeList --parsable2 -j <id> [-j <id> ...]
//
// and parses the pipe-delimited output to learn each job's state. We answer from
// qstat (live jobs) plus qacct (finished jobs, via gedata), emitting the exact
// header + row format submitit's parser expects: a "JobID|State|NodeList" header
// followed by one row per known job / array element, with unknown ids omitted so
// the consumer maps them to UNKNOWN and keeps polling.
//
// Array indexing note: array task ids are reported by 0-based position, i.e. GE
// task k is reported as <base>_<k-1> (and scancel uses the same convention). This
// is exact for the 0-based contiguous arrays submitit submits (--array=0-{n-1}).
// It does NOT reconstruct a non-zero origin or a step: sacct runs out-of-band and
// cannot read the job's SLURM_ARRAY_BASE/STEP, so for a shim array like
// --array=5-8 or 0-10:2 the reported 0-based position differs from the in-job
// SLURM_ARRAY_TASK_ID the fabricator sets. Native 1-based GE arrays are likewise
// reported one lower. (A full-fidelity squeue/sacct/scancel array view keyed on
// the true SLURM index is a possible follow-up.)
package sacct

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// header is the exact column line submitit keys on (JobID and State by name).
const header = "JobID|State|NodeList"

// Run is the sacct entry point.
func Run(args []string, stdout, stderr io.Writer) int {
	return run(gedata.ExecRunner{}, args, stdout, stderr)
}

func run(runner gedata.Runner, args []string, stdout, stderr io.Writer) int {
	ids, noHeader := parse(args)
	if !noHeader {
		fmt.Fprintln(stdout, header)
	}
	if len(ids) == 0 {
		return 0
	}

	active, err := loadActive(runner)
	if err != nil {
		fmt.Fprintf(stderr, "sacct: error: running qstat: %v\n", err)
		return 1
	}

	// submitit batches every tracked element of an array into one call
	// (`-j N_0 -j N_1 ...`); cache qacct per base so a 1000-element array is one
	// `qacct -j N`, not 1000 identical forks.
	acct := map[string][]gedata.AccountingRecord{}
	for _, id := range ids {
		for _, r := range resolve(runner, id, active, acct) {
			fmt.Fprintf(stdout, "%s|%s|%s\n", r.key, r.state, r.node)
		}
	}
	return 0
}

// parse extracts the requested job ids and whether the header is suppressed.
// Only -j/--jobs (repeatable, comma-lists accepted) selects jobs; -o/--format
// and the parsable/allocation flags are consumed or ignored because the output
// format is fixed.
func parse(args []string) (ids []string, noHeader bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-j" || a == "--jobs" || a == "--job":
			if i++; i < len(args) {
				ids = append(ids, splitIDs(args[i])...)
			}
		case strings.HasPrefix(a, "--jobs="):
			ids = append(ids, splitIDs(a[len("--jobs="):])...)
		case strings.HasPrefix(a, "--job="):
			ids = append(ids, splitIDs(a[len("--job="):])...)
		case strings.HasPrefix(a, "-j") && len(a) > 2:
			ids = append(ids, splitIDs(a[2:])...)
		case a == "-n" || a == "--noheader":
			noHeader = true
		case a == "-o" || a == "--format":
			i++ // separate value: consume it, the output columns are fixed
		}
		// Everything else is ignored, which is also the right handling for the
		// attached-value forms (-oFOO, --format=FOO): the value travels with the
		// flag, so there is nothing to consume. The format is fixed and submitit
		// only ever sends -o, --parsable2 and -j.
	}
	return ids, noHeader
}

func splitIDs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// activeRow is one live job/array task from qstat, keyed by GE base id and
// 1-based task id (0 for a non-array job).
type activeRow struct {
	base   string
	geTask int64
	state  string
	node   string
}

func loadActive(runner gedata.Runner) ([]activeRow, error) {
	out, _, exit, err := runner.Run(context.Background(), "qstat", "-xml", "-u", "*")
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, fmt.Errorf("qstat exited %d", exit)
	}
	rows, err := gedata.ParseQstatXML(out)
	if err != nil {
		return nil, err
	}
	var active []activeRow
	for _, r := range rows {
		node := ""
		if _, h, ok := strings.Cut(r.Queue, "@"); ok {
			node = h
		}
		state := gedata.AcctActiveState(r.State)
		for _, t := range expandTasks(r.TaskID) {
			active = append(active, activeRow{base: r.JobID, geTask: t, state: state, node: node})
		}
	}
	return active, nil
}

// expandTasks turns a qstat "tasks" field into the GE task ids it covers. An
// empty field is a non-array job (sentinel 0); a running array task is a single
// id. A pending array shows a range/list (e.g. "1-10:1") -- those elements are
// not terminal, so we omit them: the consumer maps a missing id to UNKNOWN and
// keeps polling until the task starts (qstat then shows it as a single id) or
// finishes (qacct then has it). This keeps GE task-range parsing out of the shim.
func expandTasks(field string) []int64 {
	field = strings.TrimSpace(field)
	if field == "" {
		return []int64{0}
	}
	if n, err := strconv.ParseInt(field, 10, 64); err == nil {
		return []int64{n}
	}
	return nil
}

type outRow struct{ key, state, node string }

// resolve produces the sacct rows for one requested id, merging live (qstat) and
// finished (qacct) state. An id may be a base ("4711") or a single 0-based array
// element ("4711_2"). Unknown tasks are simply absent from the result. acct
// caches qacct results per base across the whole invocation.
func resolve(runner gedata.Runner, id string, active []activeRow, acct map[string][]gedata.AccountingRecord) []outRow {
	base, wantTask, hasTask := splitReq(id)

	cands := map[int64]outRow{}
	live := map[int64]bool{} // GE tasks with a live qstat row (they win over qacct)
	var order []int64
	remember := func(geTask int64) {
		if _, seen := cands[geTask]; !seen {
			order = append(order, geTask)
		}
	}

	for _, a := range active {
		if a.base == base {
			remember(a.geTask)
			cands[a.geTask] = outRow{state: a.state, node: a.node}
			live[a.geTask] = true
		}
	}
	// qacct prints one record per run of a task, oldest first; the LATEST wins so a
	// task that requeued then completed reports COMPLETED, not the stale earlier
	// state. A live (qstat) row always beats any finished record.
	recs, ok := acct[base]
	if !ok {
		recs, _ = gedata.JobAccounting(context.Background(), runner, base)
		acct[base] = recs
	}
	for _, rec := range recs {
		if live[rec.TaskID] {
			continue
		}
		remember(rec.TaskID)
		cands[rec.TaskID] = outRow{state: rec.State}
	}

	isArray := false
	for _, t := range order {
		if t >= 1 {
			isArray = true
		}
	}

	var rows []outRow
	for _, t := range order {
		key := base
		if isArray {
			key = base + "_" + strconv.FormatInt(t-1, 10) // GE 1-based -> SLURM 0-based
		}
		if hasTask && (!isArray || t-1 != wantTask) {
			continue
		}
		c := cands[t]
		rows = append(rows, outRow{key: key, state: c.state, node: c.node})
	}
	return rows
}

// splitReq splits a requested id into its base and an optional SLURM (0-based)
// array task id.
func splitReq(id string) (base string, task int64, hasTask bool) {
	if i := strings.IndexByte(id, '_'); i >= 0 {
		if t, err := strconv.ParseInt(id[i+1:], 10, 64); err == nil {
			return id[:i], t, true
		}
		return id[:i], 0, false
	}
	return id, 0, false
}
