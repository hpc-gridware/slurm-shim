// Package squeue implements the squeue shim (spec sec. 7.7): it queries qstat,
// maps GE states to SLURM states, filters the jobs as squeue's options ask, and
// renders the default format, the long format, or a user -o/-O format.
package squeue

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/slurmopts"
)

// defaultFormat is SLURM's default squeue output (verified vs squeue.c):
// JOBID PARTITION NAME USER ST TIME NODES NODELIST(REASON) (REQ-SQU-002).
const defaultFormat = "%.18i %.9P %.8j %.8u %.2t %.10M %.6D %R"

// longFormat is SLURM's -l/--long output: STATE spelled out and TIME_LIMIT added.
const longFormat = "%.18i %.9P %.8j %.8u %.8T %.10M %.9l %.6D %R"

// maxDetailIDs bounds the job list handed to `qstat -xml -j`. A listing narrowed
// to a few jobs (by -j, -u, --me, -n, -t) asks about only those; past this many
// the one cluster-wide "*" query is cheaper than a huge argument list.
const maxDetailIDs = 200

type options struct {
	jobs       []string        // -j ids, each a job or an array element
	users      string          // -u list, passed to qstat -u as given
	names      map[string]bool // -n
	accounts   map[string]bool // -A
	partitions map[string]bool // -p
	states     map[string]bool // -t, compact codes; nil keeps every live state
	nodes      map[string]bool // -w
	noHeader   bool
	array      bool // -r: one row per array element
	long       bool // -l
	format     string
	formatLong bool // format is a -O field list
	// warnings name the options squeue does not translate (REQ-RUN-005).
	warnings []string
}

// getenv, currentUser and clock are seams for the specs.
var (
	getenv      = os.Getenv
	currentUser = loginName
	clock       = time.Now
)

// Run is the squeue entry point. The config error is surfaced rather than
// discarded: config.Parse returns a nil *Config on a hard error, so swallowing it
// turns a malformed config into a nil dereference instead of a diagnostic.
func Run(args []string, stdout, stderr io.Writer) int {
	cfg, warnings, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "squeue: error: loading config: %v\n", err)
		return 1
	}
	for _, w := range warnings {
		fmt.Fprintf(stderr, "squeue: warning: %s\n", w)
	}
	return run(gedata.ExecRunner{}, cfg, args, stdout, stderr)
}

func run(runner gedata.Runner, cfg *config.Config, args []string, stdout, stderr io.Writer) int {
	opt, err := parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "squeue: error: %v\n", err)
		return 1
	}
	for _, w := range opt.warnings {
		fmt.Fprintln(stderr, "squeue: warning: "+w)
	}
	toks := opt.tokens(stderr)

	rows, err := listJobs(runner, opt.users)
	if err != nil {
		fmt.Fprintf(stderr, "squeue: error: %v\n", err)
		return 1
	}
	rows = opt.keepPlain(rows)

	// Everything beyond id, name, user, state and priority comes from the
	// detail view, plain qstat -xml carries only the MASTER queue instance, so a
	// multi-node job would read as one node. Ask once, for the jobs still listed,
	// only when a column or filter needs it, and never fail the listing over it
	// -- a supplementary query is not worth a non-zero exit.
	v := view{cfg: cfg, now: clock()}
	needed := needsDetail(toks) || len(opt.accounts) > 0 || len(opt.partitions) > 0 || len(opt.nodes) > 0
	if needed && len(rows) > 0 {
		set, err := gedata.JobInfos(context.Background(), runner, detailIDs(rows))
		if err != nil {
			fmt.Fprintf(stderr, "squeue: warning: job details unavailable: %v\n", err)
		} else {
			v.hosts, v.details, v.schedInfo = set.Allocations, set.Jobs, set.SchedulerInfo
		}
		v.holdKinds = holdKindsIfNeeded(runner, v, rows, stderr)
	}
	rows = opt.keepDetailed(v, rows)
	if opt.array {
		rows = expandArrays(rows)
	}

	if !opt.noHeader {
		fmt.Fprintln(stdout, formatHeader(toks))
	}
	var guessed []string
	for _, r := range rows {
		if v.degraded(r) {
			guessed = append(guessed, slurmID(r))
		}
		fmt.Fprintln(stdout, v.formatRow(toks, r))
	}
	// A guessed allocation reads exactly like a correct single-node one, so say so
	// on stderr. Without this the only failure signal is the shape of the answer,
	// which is the same shape a real single-node job has.
	if len(guessed) > 0 && (needsHosts(toks) || len(opt.nodes) > 0) {
		fmt.Fprintf(stderr, "squeue: warning: no granted host list for %s; showing the master host only\n",
			strings.Join(guessed, ","))
	}
	return 0
}

// listJobs runs the plain `qstat -xml` listing for a comma list of users (all
// users when empty).
func listJobs(runner gedata.Runner, users string) ([]gedata.JobRow, error) {
	if users == "" {
		users = "*"
	}
	out, errOut, exit, err := runner.Run(context.Background(), "qstat", "-xml", "-u", users)
	if err != nil {
		return nil, fmt.Errorf("running qstat: %v", err)
	}
	if exit != 0 {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = fmt.Sprintf("qstat exited %d", exit)
		}
		return nil, fmt.Errorf("%s", msg)
	}
	rows, err := gedata.ParseQstatXML(out)
	if err != nil {
		return nil, fmt.Errorf("parsing qstat output: %v", err)
	}
	return rows, nil
}

// tokens resolves the output format: an explicit -o or -O (the last one given)
// wins over -l, which wins over SQUEUE_FORMAT2 and then SQUEUE_FORMAT; with none
// of them, SLURM's default.
func (o options) tokens(stderr io.Writer) []token {
	format, isLong := o.format, o.formatLong
	if format == "" {
		switch {
		case o.long:
			format = longFormat
		case getenv("SQUEUE_FORMAT2") != "":
			format, isLong = getenv("SQUEUE_FORMAT2"), true
		case getenv("SQUEUE_FORMAT") != "":
			format = getenv("SQUEUE_FORMAT")
		default:
			format = defaultFormat
		}
	}
	if !isLong {
		return tokenize(format)
	}
	toks, unknown := tokenizeLong(format)
	for _, f := range unknown {
		fmt.Fprintf(stderr, "squeue: warning: format field %s not supported, left out (slurm-shim)\n", f)
	}
	return toks
}

// keepPlain applies the filters plain qstat answers: job ids, names, states.
func (o options) keepPlain(rows []gedata.JobRow) []gedata.JobRow {
	if len(o.jobs) > 0 {
		rows = selectJobs(rows, o.jobs)
	}
	var out []gedata.JobRow
	for _, r := range rows {
		if len(o.names) > 0 && !o.names[r.Name] {
			continue
		}
		if o.states != nil && !o.states[gedata.MapState(r.State)] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// keepDetailed applies the filters that need the detail view: account,
// partition and node list. A job whose detail is missing matches none of them.
func (o options) keepDetailed(v view, rows []gedata.JobRow) []gedata.JobRow {
	var out []gedata.JobRow
	for _, r := range rows {
		if len(o.accounts) > 0 && !o.accounts[v.details[r.JobID].Account] {
			continue
		}
		if len(o.partitions) > 0 && !o.partitions[v.partition(r)] {
			continue
		}
		if len(o.nodes) > 0 && !onAny(v.hostsFor(r), o.nodes) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// detailIDs are the jobs to ask `qstat -xml -j` about: the ones still listed,
// or nil (every job) when that list would be long.
func detailIDs(rows []gedata.JobRow) []string {
	var ids []string
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.JobID] {
			seen[r.JobID] = true
			ids = append(ids, r.JobID)
		}
	}
	if len(ids) > maxDetailIDs {
		return nil
	}
	return ids
}

// expandArrays turns each pending array range into one row per element, as
// squeue -r shows them.
func expandArrays(rows []gedata.JobRow) []gedata.JobRow {
	var out []gedata.JobRow
	for _, r := range rows {
		for _, t := range taskList(r.TaskID) {
			r.TaskID = t
			out = append(out, r)
		}
	}
	return out
}

func taskList(geTask string) []string {
	if geTask == "" {
		return []string{""}
	}
	return gedata.ExpandTasks(geTask)
}

// selectJobs keeps the rows a -j list names. A job id names the whole job; an
// array element id ("4711_2", SLURM's 0-based index) names that element, and
// when it lies in a pending range the row is narrowed to it, so the listing
// shows the element that was asked about rather than the range around it.
func selectJobs(rows []gedata.JobRow, ids []string) []gedata.JobRow {
	var out []gedata.JobRow
	for _, r := range rows {
		whole := false
		var tasks []string
		for _, id := range ids {
			base, index, isElement := strings.Cut(id, "_")
			if base != r.JobID {
				continue
			}
			if !isElement {
				whole = true
				break
			}
			// An index that is not a number matches nothing, as in SLURM.
			if task, ok := gedata.GETask(index); ok && gedata.TaskListContains(r.TaskID, task) {
				tasks = append(tasks, task)
			}
		}
		switch {
		case whole:
			out = append(out, r)
		case len(tasks) > 0 && gedata.IsTaskList(r.TaskID):
			for _, t := range tasks {
				element := r
				element.TaskID = t
				out = append(out, element)
			}
		case len(tasks) > 0:
			out = append(out, r)
		}
	}
	return out
}

func onAny(hosts []string, want map[string]bool) bool {
	for _, h := range hosts {
		if want[h] {
			return true
		}
	}
	return false
}

// parse reads squeue's arguments, then fills what they left unset from the
// SQUEUE_* environment, as SLURM does (an option beats its variable).
func parse(args []string) (options, error) {
	var opt options
	var states, partitions, accounts string
	args = append([]string(nil), args...) // a flag bundle is unpacked in place
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := a, "", false
		if strings.HasPrefix(a, "--") {
			name, val, hasVal = strings.Cut(a, "=")
		} else if strings.HasPrefix(a, "-") && len(a) > 2 {
			name, val, hasVal = a[:2], a[2:], true
		}
		value := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("option %s requires a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		var v string
		if hasVal && flagLetters[name] {
			// A bundle of flags ("-hl"): take the first, look at the rest again.
			args[i] = "-" + val
			hasVal = false
			i--
		}
		switch name {
		case "-h", "--noheader":
			opt.noHeader = true
		case "-l", "--long":
			opt.long = true
		case "-r", "--array":
			opt.array = true
		case "-a", "--all":
			// Every queue is listed already; there are no hidden partitions.
		case "--me":
			opt.users = currentUser()
		case "-u", "--user":
			opt.users, err = value()
		case "-j", "--jobs":
			// The list is optional: a bare -j lists every job, as in SLURM.
			if !hasVal && (i+1 >= len(args) || strings.HasPrefix(args[i+1], "-")) {
				break
			}
			if v, err = value(); err == nil {
				opt.jobs = splitList(v)
			}
		case "-n", "--name":
			if v, err = value(); err == nil {
				opt.names = set(v)
			}
		case "-A", "--account":
			accounts, err = value()
		case "-p", "--partition":
			partitions, err = value()
		case "-t", "--states", "--state":
			states, err = value()
		case "-w", "--nodelist":
			if v, err = value(); err == nil {
				opt.nodes, err = hostSet(v)
			}
		case "-o", "--format":
			opt.format, err = value()
			opt.formatLong = false
		case "-O", "--Format":
			opt.format, err = value()
			opt.formatLong = true
		default:
			if !strings.HasPrefix(a, "-") || a == "-" {
				continue
			}
			// Not translated: say so, and step over its value (SLURM's arity)
			// rather than let it pass as nothing.
			opt.warnings = append(opt.warnings, "option "+a+" ignored (slurm-shim)")
			if squeueOpts.TakesNext(a) {
				i++
			}
		}
		if err != nil {
			return opt, err
		}
	}
	return opt, opt.fromEnv(states, partitions, accounts)
}

// flagLetters are the short options that take no value, so may be bundled.
var flagLetters = map[string]bool{"-h": true, "-l": true, "-r": true, "-a": true}

// fromEnv applies the -t, -p, -A values given, else their SQUEUE_* variables,
// and SQUEUE_USERS when neither -u nor --me was given. SQUEUE_SORT is not read:
// the listing keeps qstat's order.
func (o *options) fromEnv(states, partitions, accounts string) error {
	pick := func(given, envVar string) string {
		if given != "" {
			return given
		}
		return getenv(envVar)
	}
	if o.users == "" {
		o.users = getenv("SQUEUE_USERS")
	}
	if p := pick(partitions, "SQUEUE_PARTITION"); p != "" {
		o.partitions = set(p)
	}
	if a := pick(accounts, "SQUEUE_ACCOUNT"); a != "" {
		o.accounts = set(a)
	}
	if s := pick(states, "SQUEUE_STATES"); s != "" {
		var err error
		if o.states, err = parseStates(s); err != nil {
			return err
		}
	}
	return nil
}

// stateCodes maps every SLURM job state name and compact code squeue -t accepts
// to its compact code. Only PD, R, S and CG ever describe a job Grid Engine
// still lists; the others are valid and match nothing, as they would on a
// SLURM cluster with no job in that state.
var stateCodes = map[string]string{
	"PENDING": "PD", "RUNNING": "R", "SUSPENDED": "S", "COMPLETING": "CG",
	"COMPLETED": "CD", "CONFIGURING": "CF", "CANCELLED": "CA", "FAILED": "F",
	"TIMEOUT": "TO", "PREEMPTED": "PR", "NODE_FAIL": "NF", "BOOT_FAIL": "BF",
	"DEADLINE": "DL", "OUT_OF_MEMORY": "OOM", "REQUEUED": "RQ",
	"REQUEUE_HOLD": "RH", "REQUEUE_FED": "RF", "RESIZING": "RS",
	"RESV_DEL_HOLD": "RD", "REVOKED": "RV", "SIGNALING": "SI",
	"SPECIAL_EXIT": "SE", "STAGE_OUT": "SO", "STOPPED": "ST",
}

// parseStates reads a -t list. "all" lifts the filter (nil).
func parseStates(list string) (map[string]bool, error) {
	codes := map[string]bool{}
	for _, c := range stateCodes {
		codes[c] = true
	}
	out := map[string]bool{}
	for _, s := range splitList(list) {
		s = strings.ToUpper(s)
		if s == "ALL" {
			return nil, nil
		}
		if c, ok := stateCodes[s]; ok {
			out[c] = true
		} else if codes[s] {
			out[s] = true
		} else {
			return nil, fmt.Errorf("invalid job state specified: %s", s)
		}
	}
	return out, nil
}

func hostSet(list string) (map[string]bool, error) {
	hosts, err := encoders.ExpandNodelist(list)
	if err != nil {
		return nil, fmt.Errorf("invalid node name specified: %s", list)
	}
	out := map[string]bool{}
	for _, h := range hosts {
		out[h] = true
	}
	return out, nil
}

func splitList(list string) []string {
	var out []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func set(list string) map[string]bool {
	out := map[string]bool{}
	for _, s := range splitList(list) {
		out[s] = true
	}
	return out
}

// loginName is the caller's login name, for --me.
func loginName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

// squeueOpts is SLURM's squeue option table, for the arity of the options the
// shim does not translate.
var squeueOpts = slurmopts.For("squeue")

// holdKindsIfNeeded asks qstat for the hold kinds only when a listed job is
// held for a reason its detail does not give (OCS 9.0), once per invocation.
func holdKindsIfNeeded(runner gedata.Runner, v view, rows []gedata.JobRow, stderr io.Writer) map[string]string {
	for _, r := range rows {
		if !v.unexplainedHold(r) {
			continue
		}
		kinds, err := gedata.HoldKinds(context.Background(), runner)
		if err != nil {
			fmt.Fprintf(stderr, "squeue: warning: hold kinds unavailable: %v\n", err)
			return nil
		}
		return kinds
	}
	return nil
}
