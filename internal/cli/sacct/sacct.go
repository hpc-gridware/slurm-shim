// Package sacct implements the sacct shim (spec sec. 7.9): job accounting over
// GE's qstat (live jobs) and qacct (finished jobs, parsed by go-clusterscheduler).
//
// It is a reporting subset, not a full sacct: there is no accounting database
// behind it, so there are no job steps (.batch/.extern rows), no associations or
// QOS, and no --json. What it does support is the surface tools and humans
// actually use -- selecting jobs by id, user or time window, and choosing columns
// with --format.
//
// Exit-code note: through OCS 9.1.4 a job that ran under a PE with
// control_slaves TRUE - the setting qrsh -inherit tight integration is built on,
// so every shim PE has it - lost its script's exit status in the accounting
// record, and a job that exited non-zero of its own accord was reported
// COMPLETED/0:0. It was stock GE behavior, reproducible with plain qsub and no
// shim code, and it is fixed in OCS 9.1.5: the mapping below was already right
// and reports FAILED/3:0 there without any change. Crashes, cancellations and
// timeouts always came through GE's separate "failed" field and were reported
// correctly on both. See
// docs/solutions/integration-issues/pe-jobs-lose-exit-status-in-accounting.md.
//
// Array indexing note: array task ids are reported by 0-based position, i.e. GE
// task k is reported as <base>_<k-1> (and scancel uses the same convention). This
// is exact for the 0-based contiguous arrays submitit and Hydra submit. It does
// NOT reconstruct a non-zero origin or a step, because sacct runs out-of-band and
// cannot read the job's SLURM_ARRAY_BASE/STEP. Native 1-based GE arrays are
// likewise reported one lower.
package sacct

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// options is the parsed sacct request.
type options struct {
	ids       []string
	users     []string // -u, which SLURM accepts as a comma list
	begin     string   // qacct -b, CCYYMMDDhhmm
	end       string   // qacct -e
	beginTime time.Time
	endTime   time.Time
	states    []string // -s, normalized to long SLURM names
	format    string
	parsable  bool
	trailing  bool // -P keeps the trailing delimiter, --parsable2 does not
	noHeader  bool
}

// Run is the sacct entry point.
func Run(args []string, stdout, stderr io.Writer) int {
	return run(gedata.ExecRunner{}, args, stdout, stderr)
}

func run(runner gedata.Runner, args []string, stdout, stderr io.Writer) int {
	opt, err := parse(args)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	fields := splitFields(opt.format)
	if len(fields) == 0 {
		// A spec of only separators ("-o ,,") survives the empty-string check in
		// parse but names no columns; printing blank lines would be useless.
		fields = splitFields(defaultFormat)
	}

	// With no selector at all, sacct would report the whole accounting file.
	// Refuse rather than hammer qacct; SLURM defaults to "today for this user",
	// which needs a clock we would rather not guess at.
	if len(opt.ids) == 0 && len(opt.users) == 0 && opt.begin == "" && opt.end == "" {
		for _, line := range render(fields, nil, opt.parsable, opt.trailing, opt.noHeader) {
			fmt.Fprintln(stdout, line)
		}
		return 0
	}

	active, err := loadActive(runner)
	if err != nil {
		fmt.Fprintf(stderr, "sacct: error: running qstat: %v\n", err)
		return 1
	}

	var rows []row
	if len(opt.ids) > 0 {
		// Cache qacct per base id: submitit batches every tracked element of an
		// array into one call (-j N_0 -j N_1 ...), so a 1000-element array must
		// stay one qacct, not 1000 identical forks.
		acct := map[string][]gedata.AccountingRecord{}
		for _, id := range opt.ids {
			got, err := resolve(runner, id, active, acct)
			if err != nil {
				fmt.Fprintf(stderr, "sacct: error: running qacct: %v\n", err)
				return 1
			}
			rows = append(rows, got...)
		}
	} else {
		// qacct -o takes a single owner, so a comma list costs one call each.
		users := opt.users
		if len(users) == 0 {
			users = []string{""}
		}
		var recs []gedata.AccountingRecord
		for _, u := range users {
			got, err := gedata.JobAccounting(context.Background(), runner,
				gedata.AcctQuery{User: u, Begin: opt.begin, End: opt.end})
			if err != nil {
				fmt.Fprintf(stderr, "sacct: error: running qacct: %v\n", err)
				return 1
			}
			recs = append(recs, got...)
		}
		rows = append(rows, listRows(recs, active, opt)...)
	}
	rows = filterStates(rows, opt.states)

	for _, line := range render(fields, rows, opt.parsable, opt.trailing, opt.noHeader) {
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// parse extracts the request. Only the flags that change what is selected or
// printed are honored; the rest are ignored, since the report is derived from GE
// rather than an accounting database.
func parse(args []string) (options, error) {
	opt := options{format: defaultFormat}
	var err error
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func(prefix string) (string, bool) {
			if strings.HasPrefix(a, prefix+"=") {
				return a[len(prefix)+1:], true
			}
			return "", false
		}
		next := func() string {
			if i++; i < len(args) {
				return args[i]
			}
			return ""
		}

		switch {
		case a == "-j" || a == "--jobs" || a == "--job":
			opt.ids = append(opt.ids, splitList(next())...)
		case strings.HasPrefix(a, "-j") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			opt.ids = append(opt.ids, splitList(a[2:])...)
		case a == "-u" || a == "--user" || a == "--uid":
			opt.users = append(opt.users, splitList(next())...)
		case strings.HasPrefix(a, "-u") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			opt.users = append(opt.users, splitList(a[2:])...)
		case a == "-s" || a == "--state":
			opt.states = append(opt.states, normalizeStates(next())...)
		case strings.HasPrefix(a, "-s") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			opt.states = append(opt.states, normalizeStates(a[2:])...)
		case a == "-o" || a == "--format" || a == "--fields":
			opt.format = next()
		case strings.HasPrefix(a, "-o") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			opt.format = a[2:]
		case a == "-S" || a == "--starttime":
			if opt.beginTime, err = parseStamp(next()); err != nil {
				return opt, err
			}
			opt.begin = geStamp(opt.beginTime)
		case a == "-E" || a == "--endtime":
			if opt.endTime, err = parseStamp(next()); err != nil {
				return opt, err
			}
			opt.end = geStamp(opt.endTime)
		case a == "-n" || a == "--noheader":
			opt.noHeader = true
		case a == "-P" || a == "--parsable":
			opt.parsable, opt.trailing = true, true
		case a == "--parsable2":
			opt.parsable, opt.trailing = true, false
		case a == "-X" || a == "--allocations":
			// No-op: we never emit step rows, so allocations-only is what we do.
		default:
			for _, p := range []string{"--jobs", "--job", "--user", "--uid",
				"--format", "--fields", "--starttime", "--endtime", "--state"} {
				v, ok := val(p)
				if !ok {
					continue
				}
				switch p {
				case "--jobs", "--job":
					opt.ids = append(opt.ids, splitList(v)...)
				case "--user", "--uid":
					opt.users = append(opt.users, splitList(v)...)
				case "--format", "--fields":
					opt.format = v
				case "--state":
					opt.states = append(opt.states, normalizeStates(v)...)
				case "--starttime":
					if opt.beginTime, err = parseStamp(v); err != nil {
						return opt, err
					}
					opt.begin = geStamp(opt.beginTime)
				case "--endtime":
					if opt.endTime, err = parseStamp(v); err != nil {
						return opt, err
					}
					opt.end = geStamp(opt.endTime)
				}
			}
		}
	}
	if strings.TrimSpace(opt.format) == "" {
		opt.format = defaultFormat
	}
	return opt, nil
}

// parseStamp turns a SLURM time expression into a time. SLURM accepts far more
// spellings than these; the ones that matter for "jobs since X" are the ISO
// forms plus the named days and now[+-]<n><unit>.
//
// An unrecognized value is an ERROR, not a dropped bound. Silently discarding it
// would either widen the query to the whole accounting file or, with no other
// selector, report "no jobs" for a window the shim never applied -- and the
// caller could tell neither apart from a real answer.
func parseStamp(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	now := time.Now()
	midnight := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	}
	switch strings.ToLower(v) {
	case "now":
		return now, nil
	case "today", "midnight":
		return midnight(now), nil
	case "yesterday":
		return midnight(now.AddDate(0, 0, -1)), nil
	case "tomorrow":
		return midnight(now.AddDate(0, 0, 1)), nil
	}
	if d, ok := parseNowOffset(v); ok {
		return now.Add(d), nil
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02",
		"01/02-15:04:05", "01/02-15:04", "15:04:05", "15:04",
	} {
		t, err := time.ParseInLocation(layout, v, now.Location())
		if err != nil {
			continue
		}
		// The time-only and MM/DD layouts default the missing fields to year
		// zero; fill them in from today, as SLURM does.
		if t.Year() == 0 {
			if t.Month() == 1 && t.Day() == 1 && !strings.Contains(v, "/") {
				t = time.Date(now.Year(), now.Month(), now.Day(),
					t.Hour(), t.Minute(), t.Second(), 0, now.Location())
			} else {
				t = t.AddDate(now.Year(), 0, 0)
			}
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("sacct: error: unrecognized time value %q", v)
}

// parseNowOffset handles SLURM's relative forms, e.g. now-2hours, now+30minutes.
func parseNowOffset(v string) (time.Duration, bool) {
	l := strings.ToLower(v)
	if !strings.HasPrefix(l, "now+") && !strings.HasPrefix(l, "now-") {
		return 0, false
	}
	sign := time.Duration(1)
	if l[3] == '-' {
		sign = -1
	}
	rest := l[4:]
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(rest[:i], 10, 64)
	if err != nil {
		return 0, false
	}
	unit := strings.TrimSuffix(strings.TrimSpace(rest[i:]), "s")
	var scale time.Duration
	switch unit {
	case "second", "sec":
		scale = time.Second
	case "minute", "min", "":
		scale = time.Minute // SLURM's bare number means minutes
	case "hour", "hr":
		scale = time.Hour
	case "day":
		scale = 24 * time.Hour
	case "week":
		scale = 7 * 24 * time.Hour
	default:
		return 0, false
	}
	return sign * time.Duration(n) * scale, true
}

// geStamp renders a time as qacct's CCYYMMDDhhmm, or "" for the zero time.
func geStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("200601021504")
}

// normalizeStates maps a -s value onto the long SLURM state names this shim
// reports. An unrecognized name is kept as-is (upper-cased) so it simply matches
// nothing rather than silently widening the selection.
func normalizeStates(spec string) []string {
	long := map[string]string{
		"pd": "PENDING", "pending": "PENDING",
		"r": "RUNNING", "running": "RUNNING",
		"s": "SUSPENDED", "suspended": "SUSPENDED",
		"cg": "COMPLETING", "completing": "COMPLETING",
		"cd": "COMPLETED", "completed": "COMPLETED",
		"f": "FAILED", "failed": "FAILED",
		"ca": "CANCELLED", "cancelled": "CANCELLED", "canceled": "CANCELLED",
		"to": "TIMEOUT", "timeout": "TIMEOUT",
		"nf": "NODE_FAIL", "node_fail": "NODE_FAIL",
		"rq": "REQUEUED", "requeued": "REQUEUED",
	}
	var out []string
	for _, p := range splitList(spec) {
		if l, ok := long[strings.ToLower(p)]; ok {
			out = append(out, l)
		} else {
			out = append(out, strings.ToUpper(p))
		}
	}
	return out
}

// filterStates keeps only the rows whose state was asked for. An empty selection
// keeps everything.
func filterStates(rows []row, states []string) []row {
	if len(states) == 0 {
		return rows
	}
	want := make(map[string]bool, len(states))
	for _, s := range states {
		want[s] = true
	}
	out := rows[:0]
	for _, r := range rows {
		if want[r.state] {
			out = append(out, r)
		}
	}
	return out
}

// splitList splits a comma-separated flag value, dropping empty entries so a
// trailing comma is harmless.
func splitList(s string) []string {
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
	base    string
	geTask  int64
	state   string
	node    string
	name    string
	user    string
	queue   string
	slots   int64
	start   time.Time
	submit  time.Time
	isArray bool
}

// row builds the output row for a live job. Only what qstat actually knows is
// filled in: usage and the exit code are written at the end of the job, so
// leaving them empty is the honest answer. Elapsed is time on the clock so far,
// which is what sacct reports for a running job.
func (a activeRow) row() row {
	r := row{
		jobName: a.name, partition: a.queue, allocCPUS: a.slots,
		state: a.state, nodeList: a.node, user: a.user,
		start: a.start, submit: a.submit,
	}
	if !a.start.IsZero() {
		if d := time.Since(a.start); d > 0 {
			r.elapsed = d
		}
	}
	return r
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
		node, queue := "", r.Queue
		if q, h, ok := strings.Cut(r.Queue, "@"); ok {
			queue, node = q, h
		}
		state := gedata.AcctActiveState(r.State)
		tasks, isArray := expandTasks(r.TaskID)
		for _, t := range tasks {
			active = append(active, activeRow{
				base: r.JobID, geTask: t, state: state, node: node,
				name: r.Name, user: r.User, queue: queue,
				slots: int64(r.Slots), start: r.Start, submit: r.Submit,
				isArray: isArray,
			})
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
func expandTasks(field string) ([]int64, bool) {
	field = strings.TrimSpace(field)
	if field == "" {
		return []int64{0}, false
	}
	if n, err := strconv.ParseInt(field, 10, 64); err == nil {
		return []int64{n}, true
	}
	return nil, true
}

// resolve produces the rows for one requested id, merging live (qstat) and
// finished (qacct) state. An id may be a base ("4711") or a single 0-based array
// element ("4711_2"). Unknown tasks are simply absent from the result. acct
// caches qacct results per base across the whole invocation.
func resolve(runner gedata.Runner, id string, active []activeRow, acct map[string][]gedata.AccountingRecord) ([]row, error) {
	base, wantTask, hasTask, ok := splitReq(id)
	if !ok {
		return nil, nil
	}

	cands := map[int64]row{}
	live := map[int64]bool{}
	var order []int64
	remember := func(geTask int64) {
		if _, seen := cands[geTask]; !seen {
			order = append(order, geTask)
		}
	}

	arrayHint := false
	for _, a := range active {
		if a.base != base {
			continue
		}
		remember(a.geTask)
		cands[a.geTask] = a.row()
		live[a.geTask] = true
		if a.isArray {
			arrayHint = true
		}
	}

	// qacct prints one record per run of a task, oldest first; the LATEST wins so
	// a task that requeued then completed reports COMPLETED, not the stale
	// earlier state. A live (qstat) row always beats any finished record.
	//
	// A qacct that cannot run must NOT be mistaken for "no record": an empty
	// result means "unknown, keep polling", so swallowing the error would turn
	// every finished job into a permanent non-terminal answer with exit 0 -- the
	// one outcome a polling consumer cannot recover from.
	recs, ok := acct[base]
	if !ok {
		var err error
		recs, err = gedata.JobAccounting(context.Background(), runner, gedata.AcctQuery{JobID: base})
		if err != nil {
			return nil, err
		}
		acct[base] = recs
	}
	for _, rec := range recs {
		// qacct given a non-numeric id treats it as a job NAME and answers with
		// every job that matches, so a record's own number is the only reliable
		// key. Without this check an unrelated job's record would overwrite the
		// requested one whenever both carry the same task id.
		if strconv.FormatInt(rec.JobNumber, 10) != base {
			continue
		}
		if live[rec.TaskID] {
			continue
		}
		remember(rec.TaskID)
		cands[rec.TaskID] = acctRow(rec)
	}

	isArray := arrayHint
	for _, t := range order {
		if t >= 1 {
			isArray = true
		}
	}

	var out []row
	for _, t := range order {
		if hasTask && (!isArray || t-1 != wantTask) {
			continue
		}
		r := cands[t]
		// Task 0 is the non-array sentinel even when array siblings are present
		// (a reused job number can put both in one qacct answer); rendering it
		// as an array element would emit the nonexistent id "<base>_-1".
		r.jobID = elementID(base, t, isArray && t >= 1)
		out = append(out, r)
	}
	return out, nil
}

// listRows renders records selected by user/time rather than by id, overlaying
// any that are still running.
//
// The window has to be applied to the live rows here as well as to qacct: qstat
// answers for the whole cluster right now, so without this a query bounded to
// last night would still list everything running at this moment.
func listRows(recs []gedata.AccountingRecord, active []activeRow, opt options) []row {
	users := map[string]bool{}
	for _, u := range opt.users {
		users[u] = true
	}
	live := map[string]bool{}
	var out []row
	for _, a := range active {
		if len(users) > 0 && !users[a.user] {
			continue
		}
		if !inWindow(a, opt) {
			continue
		}
		live[a.base+":"+strconv.FormatInt(a.geTask, 10)] = true
		r := a.row()
		r.jobID = elementID(a.base, a.geTask, a.isArray)
		out = append(out, r)
	}
	for _, rec := range recs {
		base := strconv.FormatInt(rec.JobNumber, 10)
		if live[base+":"+strconv.FormatInt(rec.TaskID, 10)] {
			continue
		}
		r := acctRow(rec)
		r.jobID = elementID(base, rec.TaskID, rec.TaskID >= 1)
		out = append(out, r)
	}
	return out
}

// inWindow reports whether a live job falls inside the requested -S/-E bounds.
// A job that is still running has not ended, so it is in the window as long as
// it started before the end bound; a pending job is judged by its submit time.
// A job with neither timestamp is kept rather than guessed away.
func inWindow(a activeRow, opt options) bool {
	when := a.start
	if when.IsZero() {
		when = a.submit
	}
	if when.IsZero() {
		return true
	}
	if !opt.endTime.IsZero() && when.After(opt.endTime) {
		return false
	}
	// A job that started before the window but is STILL running overlaps it, so
	// only a job that also ended before the window can be excluded -- and a live
	// job by definition has not ended.
	return true
}

// acctRow adapts a finished record; the job id is filled in by the caller, which
// knows whether the job is an array.
func acctRow(rec gedata.AccountingRecord) row {
	return row{
		jobName: rec.JobName, partition: rec.Queue, account: rec.Account,
		allocCPUS: rec.Slots, state: rec.State, exitCode: rec.ExitCode,
		nodeList: rec.Host, user: rec.User, submit: rec.Submit,
		start: rec.Start, end: rec.End, elapsed: rec.Elapsed,
		maxRSS: rec.MaxRSS, totalCPU: rec.TotalCPU,
	}
}

// elementID renders the SLURM-facing job id, converting GE's 1-based array task
// to the 0-based element SLURM tools expect.
func elementID(base string, geTask int64, isArray bool) string {
	if !isArray {
		return base
	}
	return base + "_" + strconv.FormatInt(geTask-1, 10)
}

// splitReq splits a requested id into its base and an optional SLURM (0-based)
// array task id. ok is false for a suffix that names no element this shim can
// report -- a step id ("4712_0.batch"), a range ("4712_[0-3]") or junk -- so the
// caller reports nothing rather than silently widening the request to the whole
// array.
func splitReq(id string) (base string, task int64, hasTask, ok bool) {
	i := strings.IndexByte(id, '_')
	if i < 0 {
		// A plain step id on a non-array job ("4711.batch") names no job either.
		if strings.ContainsRune(id, '.') {
			return id, 0, false, false
		}
		return id, 0, false, true
	}
	t, err := strconv.ParseInt(id[i+1:], 10, 64)
	if err != nil {
		return id[:i], 0, false, false
	}
	return id[:i], t, true, true
}
