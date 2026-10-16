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

// Package sacct implements the sacct shim (spec sec. 7.9): job accounting over
// GE's qstat (live jobs) and qacct (finished jobs, parsed by go-clusterscheduler).
//
// It is a reporting subset, not a full sacct: there is no accounting database
// behind it, so there are no job steps (.batch/.extern rows), no associations or
// QOS, and no --json. What it does support is the surface tools and humans
// actually use -- selecting jobs by id, user, name or time window, and choosing
// columns with --format, -b or SACCT_FORMAT.
//
// Request columns note: Timelimit, ReqMem and WorkDir come from the qsub command
// line qacct records, the only place qacct shows a job's requests; NNodes comes
// from the -par geometry or the PE task records. See request.go for what that
// can and cannot see.
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
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/slurmopts"
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
	names     []string // --name, exact job names
	format    string
	parsable  bool
	trailing  bool   // -p keeps the trailing delimiter, -P/--parsable2 does not
	delimiter string // --delimiter, for -p/-P output
	noHeader  bool
	allUsers  bool // -a: do not narrow a listing to the caller's own jobs
	dups      bool // -D: every run of a requeued job, not just the latest
	// warnings name the options sacct does not translate (REQ-RUN-005).
	warnings []string
}

// sacctOpts is SLURM's sacct option table, for the arity of the options the
// shim does not translate.
var sacctOpts = slurmopts.For("sacct")

// briefFields is SLURM's -b/--brief column set.
const briefFields = "JobID,State,ExitCode"

// getenv reads SACCT_FORMAT; a variable so specs can set it without touching
// the process environment.
var getenv = os.Getenv

// whoami returns the caller's login name and whether the caller is root. A
// variable so specs can play an ordinary user or root.
var whoami = func() (string, bool) {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return name, os.Getuid() == 0
}

// Run is the sacct entry point. A config that cannot be loaded falls back to the
// built-in defaults: sacct only needs the memory complex name for ReqMem, and a
// polling consumer must not lose its job states over a cosmetic column.
func Run(args []string, stdout, stderr io.Writer) int {
	memComplex := config.Default().MemoryComplex
	if cfg, _, err := config.Load(); err == nil && cfg != nil {
		memComplex = cfg.MemoryComplex
	}
	return runWith(gedata.ExecRunner{}, memComplex, args, stdout, stderr)
}

// run is the spec seam with the built-in memory complex.
func run(runner gedata.Runner, args []string, stdout, stderr io.Writer) int {
	return runWith(runner, config.Default().MemoryComplex, args, stdout, stderr)
}

func runWith(runner gedata.Runner, memComplex string, args []string, stdout, stderr io.Writer) int {
	opt, err := parse(args)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	for _, w := range opt.warnings {
		fmt.Fprintln(stderr, "sacct: warning: "+w)
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
	if len(opt.ids) == 0 && len(opt.users) == 0 && len(opt.names) == 0 && opt.begin == "" && opt.end == "" {
		for _, line := range render(fields, nil, opt) {
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
			got, err := resolve(runner, id, active, acct, opt.dups, memComplex)
			if err != nil {
				fmt.Fprintf(stderr, "sacct: error: running qacct: %v\n", err)
				return 1
			}
			rows = append(rows, got...)
		}
	} else {
		opt.users = listUsers(opt)
		// qacct -o takes a single owner and -j a single name (qacct reads a
		// non-numeric -j as a job name), so each combination costs one call.
		users, names := orAll(opt.users), orAll(opt.names)
		var recs []gedata.AccountingRecord
		for _, u := range users {
			for _, n := range names {
				got, err := gedata.JobAccounting(context.Background(), runner,
					gedata.AcctQuery{JobID: n, User: u, Begin: opt.begin, End: opt.end})
				if err != nil {
					fmt.Fprintf(stderr, "sacct: error: running qacct: %v\n", err)
					return 1
				}
				recs = append(recs, got...)
			}
		}
		rows = append(rows, listRows(recs, active, opt, memComplex)...)
	}
	rows = filterStates(rows, opt.states)
	rows = filterNames(rows, opt.names)

	for _, line := range render(fields, rows, opt) {
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// parse extracts the request. Only the flags that change what is selected or
// printed are honored; the rest are ignored, since the report is derived from GE
// rather than an accounting database.
func parse(args []string) (options, error) {
	opt := options{delimiter: "|"}
	var formats []string
	brief := false
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
			formats = append(formats, next())
		case strings.HasPrefix(a, "-o") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			formats = append(formats, a[2:])
		case a == "--name":
			opt.names = append(opt.names, splitList(next())...)
		case a == "--delimiter":
			opt.delimiter = next()
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
		case a == "-p" || a == "--parsable":
			opt.parsable, opt.trailing = true, true
		case a == "-P" || a == "--parsable2":
			opt.parsable, opt.trailing = true, false
		case a == "-a" || a == "--allusers":
			opt.allUsers = true
		case a == "-D" || a == "--duplicates":
			opt.dups = true
		case a == "-b" || a == "--brief":
			brief = true
		case a == "-X" || a == "--allocations":
			// No-op: we never emit step rows, so allocations-only is what we do.
		default:
			matched := false
			for _, p := range []string{"--jobs", "--job", "--user", "--uid",
				"--format", "--fields", "--starttime", "--endtime", "--state",
				"--name", "--delimiter"} {
				v, ok := val(p)
				if !ok {
					continue
				}
				matched = true
				switch p {
				case "--jobs", "--job":
					opt.ids = append(opt.ids, splitList(v)...)
				case "--user", "--uid":
					opt.users = append(opt.users, splitList(v)...)
				case "--format", "--fields":
					formats = append(formats, v)
				case "--name":
					opt.names = append(opt.names, splitList(v)...)
				case "--delimiter":
					opt.delimiter = v
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
			if !matched && strings.HasPrefix(a, "-") && a != "-" {
				// Not translated: say so, and step over its value (SLURM's
				// arity) rather than let it pass as nothing.
				opt.warnings = append(opt.warnings, "option "+a+" ignored (slurm-shim)")
				if sacctOpts.TakesNext(a) {
					i++
				}
			}
		}
	}
	// A name or user reaches qacct as the value of -j or -o; one starting
	// with "-" would be read as an option of its own.
	for _, v := range slices.Concat(opt.names, opt.users) {
		if strings.HasPrefix(v, "-") {
			return opt, fmt.Errorf("invalid job or user name %q", v)
		}
	}
	opt.format = fieldList(formats, brief)
	return opt, nil
}

// fieldList builds the column list the way SLURM's sacct does: every -o is
// appended in order, -b appends the brief set after them, and only when neither
// was given does SACCT_FORMAT (then the built-in default) apply.
func fieldList(formats []string, brief bool) string {
	if brief {
		formats = append(formats, briefFields)
	}
	if len(formats) > 0 {
		if f := strings.Join(formats, ","); len(splitFields(f)) > 0 {
			return f
		}
		return defaultFormat
	}
	if env := strings.TrimSpace(getenv("SACCT_FORMAT")); env != "" {
		return env
	}
	return defaultFormat
}

// listUsers returns the owners a listing (no -j) is narrowed to. SLURM shows an
// ordinary user only their own jobs unless -a or -u says otherwise; root sees
// everyone. A caller whose name cannot be resolved is not narrowed, since an
// empty owner would match nothing and read as "no jobs".
func listUsers(opt options) []string {
	if len(opt.users) > 0 || opt.allUsers {
		return opt.users
	}
	name, root := whoami()
	if root || name == "" {
		return nil
	}
	return []string{name}
}

// orAll turns an empty filter into the single "no filter" value.
func orAll(list []string) []string {
	if len(list) == 0 {
		return []string{""}
	}
	return list
}

// filterNames keeps only rows whose job name is one of names; SLURM matches
// --name exactly. qacct's own -j <name> also matches patterns, so this check is
// what makes the answer exact. An empty selection keeps everything.
func filterNames(rows []row, names []string) []row {
	if len(names) == 0 {
		return rows
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := rows[:0]
	for _, r := range rows {
		if want[r.jobName] {
			out = append(out, r)
		}
	}
	return out
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
func resolve(runner gedata.Runner, id string, active []activeRow, acct map[string][]gedata.AccountingRecord,
	dups bool, memComplex string) ([]row, error) {
	base, wantTask, hasTask, ok := splitReq(id)
	if !ok {
		return nil, nil
	}

	// runs holds every finished run of a task, oldest first; liveRow its qstat
	// row, if any.
	runs := map[int64][]row{}
	liveRow := map[int64]row{}
	seen := map[int64]bool{}
	var order []int64
	remember := func(geTask int64) {
		if !seen[geTask] {
			seen[geTask] = true
			order = append(order, geTask)
		}
	}

	arrayHint := false
	for _, a := range active {
		if a.base != base {
			continue
		}
		remember(a.geTask)
		liveRow[a.geTask] = a.row()
		if a.isArray {
			arrayHint = true
		}
	}

	// qacct prints one record per run of a task, oldest first; the LATEST wins so
	// a task that requeued then completed reports COMPLETED, not the stale
	// earlier state. A live (qstat) row always beats any finished record. -D
	// shows every run instead, oldest first, the way SLURM lists a requeued job.
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
		remember(rec.TaskID)
		runs[rec.TaskID] = append(runs[rec.TaskID], acctRow(rec, memComplex))
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
		// Task 0 is the non-array sentinel even when array siblings are present
		// (a reused job number can put both in one qacct answer); rendering it
		// as an array element would emit the nonexistent id "<base>_-1".
		jobID := elementID(base, t, isArray && t >= 1)
		for _, r := range taskRows(runs[t], liveRow, t, dups) {
			r.jobID = jobID
			out = append(out, r)
		}
	}
	return out, nil
}

// taskRows picks what to print for one task: without -D the live row, else the
// latest run; with -D every finished run, then the live row.
func taskRows(runs []row, liveRow map[int64]row, task int64, dups bool) []row {
	live, isLive := liveRow[task]
	if !dups {
		if isLive {
			return []row{live}
		}
		if len(runs) == 0 {
			return nil
		}
		return runs[len(runs)-1:]
	}
	out := append([]row(nil), runs...)
	if isLive {
		out = append(out, live)
	}
	return out
}

// listRows renders records selected by user/time rather than by id, overlaying
// any that are still running.
//
// The window has to be applied to the live rows here as well as to qacct: qstat
// answers for the whole cluster right now, so without this a query bounded to
// last night would still list everything running at this moment.
func listRows(recs []gedata.AccountingRecord, active []activeRow, opt options, memComplex string) []row {
	users := map[string]bool{}
	for _, u := range opt.users {
		users[u] = true
	}
	names := map[string]bool{}
	for _, n := range opt.names {
		names[n] = true
	}
	live := map[string]bool{}
	var out []row
	for _, a := range active {
		if len(users) > 0 && !users[a.user] {
			continue
		}
		if len(names) > 0 && !names[a.name] {
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
	// Without -D only the latest run of each task is shown, and none at all for
	// a task that is running again; qacct lists runs oldest first.
	latest := map[string]int{}
	for i, rec := range recs {
		latest[recKey(rec)] = i
	}
	for i, rec := range recs {
		base := strconv.FormatInt(rec.JobNumber, 10)
		if !opt.dups && (live[recKey(rec)] || latest[recKey(rec)] != i) {
			continue
		}
		r := acctRow(rec, memComplex)
		r.jobID = elementID(base, rec.TaskID, rec.TaskID >= 1)
		out = append(out, r)
	}
	return out
}

// recKey identifies a finished record's job and task, matching the live keys.
func recKey(rec gedata.AccountingRecord) string {
	return strconv.FormatInt(rec.JobNumber, 10) + ":" + strconv.FormatInt(rec.TaskID, 10)
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
func acctRow(rec gedata.AccountingRecord, memComplex string) row {
	req := parseSubmitLine(rec.SubmitCmdLine, memComplex)
	return row{
		jobName: rec.JobName, partition: rec.Queue, account: rec.Account,
		allocCPUS: rec.Slots, state: finishedState(rec, req), exitCode: rec.ExitCode,
		nodeList: rec.Host, user: rec.User, submit: rec.Submit,
		start: rec.Start, end: rec.End, elapsed: rec.Elapsed,
		maxRSS: rec.MaxRSS, totalCPU: rec.TotalCPU,
		nnodes: nodeCount(rec, req), reqMem: req.reqMem(), timelimit: req.timelimit(),
		workDir: req.workDir,
	}
}

// finishedState refines GE's CANCELLED into SLURM's TIMEOUT for a job killed at
// its wall-clock limit. Verified on OCS 9.1.6: a job submitted -l h_rt=5 that
// sleeps 60 is recorded failed=100, exit_status=137 -- exactly what a qdel
// leaves -- and only its run time (ru_wallclock 6) tells the two apart. A qdel
// can only land before the limit, so a run that reached the requested h_rt was
// ended by it.
func finishedState(rec gedata.AccountingRecord, req jobRequest) string {
	if rec.State == "CANCELLED" && req.hasTimeLimit && req.timeLimit > 0 &&
		rec.Elapsed >= req.timeLimit {
		return "TIMEOUT"
	}
	return rec.State
}

// nodeCount is NNodes for a finished run, or "" when the record cannot say.
// A job without a PE ran on one host. A PE job is counted from the geometry it
// requested when that fixes it (-par N on a multiple of N slots, or $pe_slots),
// and otherwise from the hosts its PE task records ran on. With neither, the
// granted hosts are not in the accounting record, so the answer is empty.
func nodeCount(rec gedata.AccountingRecord, req jobRequest) string {
	if rec.GrantedPE == "" || rec.Slots == 1 || req.perHost == "$pe_slots" {
		return "1"
	}
	if n, err := strconv.ParseInt(req.perHost, 10, 64); err == nil && n > 0 && rec.Slots%n == 0 {
		return strconv.FormatInt(rec.Slots/n, 10)
	}
	if rec.PETasks > 0 {
		return strconv.Itoa(len(rec.Hosts))
	}
	return ""
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
