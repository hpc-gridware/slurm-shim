// Package scancel implements the scancel shim (spec sec. 7.8): it maps job and
// array-task cancellation to qdel. `scancel --signal` (submitit's
// checkpoint-preempt) maps instead to a GE reschedule (qmod -rj), which delivers
// SIGUSR2 to a -notify job and restarts it, so the job checkpoints and resumes.
package scancel

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/slurmopts"
)

// Run is the scancel entry point. Under SLURM_SHIM_DRY_RUN the runner reports the
// qdel/qmod it would run rather than cancelling anything, on stderr -- real
// scancel writes nothing to stdout, and a caller must not mistake the report for
// output. The banner is printed up front so a run that reaches no mutating client
// is still distinguishable from a real cancellation.
func Run(args []string, stdout, stderr io.Writer) int {
	if dryrun.Enabled() {
		fmt.Fprintln(stderr, dryrun.Banner("scancel"))
	}
	return run(dryrun.Wrap(gedata.ExecRunner{}, stderr, "scancel"), args, stdout, stderr)
}

// scancelOpts is SLURM's scancel option table, for the arity of the options
// the shim does not translate.
var scancelOpts = slurmopts.For("scancel")

// request is a parsed scancel command line. As in SLURM, every given filter
// must match: ids narrow the jobs, -u/--me/-n/-t/-p/-w narrow them further.
type request struct {
	ids        []string
	user       string
	name       string
	states     map[string]bool // compact SLURM states: PD, R, S, CG
	partitions map[string]bool
	nodes      map[string]bool
	signal     string // "" cancels
}

func (r request) filtered() bool {
	return r.user != "" || r.name != "" || len(r.states) > 0 || len(r.partitions) > 0 || len(r.nodes) > 0
}

func run(runner gedata.Runner, args []string, stdout, stderr io.Writer) int {
	req, err := parse(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "scancel: error: "+err.Error())
		return 1
	}
	if len(req.ids) == 0 && !req.filtered() {
		fmt.Fprintln(stderr, "scancel: error: no job id given")
		return 1
	}
	act, err := signalAction(req.signal)
	if err != nil {
		fmt.Fprintln(stderr, "scancel: error: "+err.Error())
		return 1
	}

	// No filter: act on the ids as given, one client call each, as before.
	if !req.filtered() {
		return forEach(req.ids, func(id string) int { return act(runner, stderr, id) })
	}
	// Only -u, no ids, plain cancel: qdel -u is the same and atomic.
	if req.user != "" && len(req.ids) == 0 && req.name == "" && len(req.states) == 0 &&
		len(req.partitions) == 0 && len(req.nodes) == 0 && req.signal == "" {
		return qdel(runner, stderr, "-u", req.user)
	}
	targets, err := matching(runner, req)
	if err != nil {
		fmt.Fprintln(stderr, "scancel: error: "+err.Error())
		return 1
	}
	// Ids that match no filter are skipped silently, as in SLURM.
	return forEach(targets, func(id string) int { return act(runner, stderr, id) })
}

// parse reads scancel's options. An option the shim does not translate warns
// and steps over its value (SLURM's arity), so the value is never taken for a
// job id.
func parse(args []string, stderr io.Writer) (request, error) {
	var req request
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
		switch name {
		case "-u", "--user":
			req.user, err = value()
		case "--me":
			req.user = currentUser()
		case "-n", "--name", "--jobname":
			req.name, err = value()
		case "-t", "--state":
			var v string
			if v, err = value(); err == nil {
				req.states, err = parseStates(v)
			}
		case "-p", "--partition":
			var v string
			if v, err = value(); err == nil {
				req.partitions = set(v)
			}
		case "-w", "--nodelist":
			var v string
			if v, err = value(); err == nil {
				var hosts []string
				if hosts, err = encoders.ExpandNodelist(v); err == nil {
					req.nodes = map[string]bool{}
					for _, h := range hosts {
						req.nodes[h] = true
					}
				}
			}
		case "-s", "--signal":
			req.signal, err = value()
		case "-Q", "--quiet", "-v", "--verbose", "-b", "--batch", "-f", "--full":
			// No effect here: every signal goes to the whole job (see signalAction).
		default:
			if !strings.HasPrefix(a, "-") || a == "-" {
				req.ids = append(req.ids, a)
				continue
			}
			fmt.Fprintf(stderr, "scancel: warning: option %s ignored (slurm-shim)\n", a)
			if scancelOpts.TakesNext(a) {
				i++
			}
		}
		if err != nil {
			return req, err
		}
	}
	return req, nil
}

// action applies a cancel or a signal to one SLURM job id ("4711", "4711_2").
type action func(runner gedata.Runner, stderr io.Writer, id string) int

// signalAction maps scancel -s to what Grid Engine can do to a job. It has no
// way to send an arbitrary signal, so only these map:
//
//   - none: cancel (qdel).
//   - TERM, USR1, USR2: reschedule (qmod -rj). A job submitted with --signal
//     runs with -notify -r y, so it receives SIGUSR2 and restarts; this is the
//     checkpoint-preempt submitit drives with `scancel --signal TERM`, then USR2.
//   - STOP, CONT: suspend and resume (qmod -sj, -usj).
//   - KILL: cancel.
//
// Any other signal is an error rather than a guess.
func signalAction(sig string) (action, error) {
	if sig == "" {
		return func(r gedata.Runner, e io.Writer, id string) int { return qdel(r, e, qdelArgs(id)...) }, nil
	}
	switch name := signalName(sig); name {
	case "TERM", "USR1", "USR2":
		return reschedule, nil
	case "STOP":
		return func(r gedata.Runner, e io.Writer, id string) int { return qmod(r, e, "-sj", id) }, nil
	case "CONT":
		return func(r gedata.Runner, e io.Writer, id string) int { return qmod(r, e, "-usj", id) }, nil
	case "KILL":
		return func(r gedata.Runner, e io.Writer, id string) int { return qdel(r, e, qdelArgs(id)...) }, nil
	default:
		return nil, fmt.Errorf("signal %s has no Grid Engine equivalent; "+
			"supported: TERM, USR1, USR2 (reschedule), STOP, CONT (suspend, resume), KILL (cancel)", sig)
	}
}

// signalName normalizes "SIGUSR2", "usr2" or a Linux signal number to "USR2".
func signalName(sig string) string {
	s := strings.ToUpper(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(sig)), "SIG"))
	byNumber := map[string]string{"9": "KILL", "10": "USR1", "12": "USR2", "15": "TERM", "18": "CONT", "19": "STOP"}
	if n, ok := byNumber[s]; ok {
		return n
	}
	return s
}

// parseStates reads -t: comma-separated long or compact SLURM states.
func parseStates(v string) (map[string]bool, error) {
	long := map[string]string{
		"PENDING": "PD", "RUNNING": "R", "SUSPENDED": "S", "COMPLETING": "CG",
		"PD": "PD", "R": "R", "S": "S", "CG": "CG",
	}
	out := map[string]bool{}
	for _, s := range strings.Split(v, ",") {
		c, ok := long[strings.ToUpper(strings.TrimSpace(s))]
		if !ok {
			return nil, fmt.Errorf("invalid job state %q (pending, running, suspended, completing)", s)
		}
		out[c] = true
	}
	return out, nil
}

// matching lists the jobs that pass every filter and returns their SLURM ids:
// a job id, or for an array the matching elements one by one, so a filter on a
// half-running array cancels only the matching tasks. The listing and the
// cancel are two steps, so a job that changes state in between is acted on as
// listed.
func matching(runner gedata.Runner, req request) ([]string, error) {
	user := req.user
	if user == "" {
		user = "*"
	}
	out, errOut, exit, err := runner.Run(context.Background(), "qstat", "-xml", "-u", user)
	if err != nil {
		return nil, fmt.Errorf("running qstat: %v", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("qstat: %s", strings.TrimSpace(string(errOut)))
	}
	rows, err := gedata.ParseQstatXML(out)
	if err != nil {
		return nil, fmt.Errorf("parsing qstat output: %v", err)
	}
	var hosts map[string]gedata.JobAllocation
	if len(req.nodes) > 0 {
		if hosts, err = gedata.JobAllocations(context.Background(), runner, ""); err != nil {
			return nil, fmt.Errorf("listing job hosts: %v", err)
		}
	}
	queues := map[string]bool{}
	if len(req.partitions) > 0 {
		cfg, _, err := config.Load()
		if err != nil {
			return nil, fmt.Errorf("loading config: %v", err)
		}
		for p := range req.partitions {
			for _, q := range cfg.QueuesOf(p) {
				queues[q] = true
			}
		}
	}

	var targets []string
	for _, r := range rows {
		if req.name != "" && r.Name != req.name {
			continue
		}
		if len(req.states) > 0 && !req.states[gedata.MapState(r.State)] {
			continue
		}
		if cluster, _, _ := strings.Cut(r.Queue, "@"); len(queues) > 0 && !queues[cluster] {
			continue // a pending job has no queue yet, so it matches no partition
		}
		if len(req.nodes) > 0 && !onAny(hosts[r.Key()].Hosts, req.nodes) {
			continue
		}
		for _, id := range elements(r) {
			if len(req.ids) == 0 || named(req.ids, id) {
				targets = append(targets, id)
			}
		}
	}
	return targets, nil
}

// elements are a qstat row's SLURM ids: the job, one array element, or every
// element of a pending range.
func elements(r gedata.JobRow) []string {
	if r.TaskID == "" {
		return []string{r.JobID}
	}
	if _, err := strconv.Atoi(r.TaskID); err == nil {
		return []string{r.JobID + "_" + gedata.SlurmTask(r.TaskID)}
	}
	var out []string
	for t := 1; t <= maxArrayTask; t++ {
		if gedata.TaskInRange(r.TaskID, strconv.Itoa(t)) {
			out = append(out, r.JobID+"_"+strconv.Itoa(t-1))
		}
	}
	return out
}

// maxArrayTask bounds the expansion of a pending range (GE's max_aj_tasks
// default is 75000).
const maxArrayTask = 75000

// named reports whether the user's ids name id: the same id, or the whole job
// an array element belongs to.
func named(ids []string, id string) bool {
	base, _, _ := strings.Cut(id, "_")
	for _, want := range ids {
		if want == id || want == base {
			return true
		}
	}
	return false
}

func onAny(hosts []string, want map[string]bool) bool {
	for _, h := range hosts {
		if want[h] {
			return true
		}
	}
	return false
}

func set(list string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}

func forEach(ids []string, f func(string) int) int {
	worst := 0
	for _, id := range ids {
		if code := f(id); code != 0 {
			worst = code
		}
	}
	return worst
}

// currentUser is the caller's login name, for --me.
func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
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
		if task, ok := gedata.GETask(id[i+1:]); ok {
			target = id[:i] + "." + task
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

// qmod suspends or resumes one SLURM job id (an array element as N.<k+1>).
func qmod(runner gedata.Runner, stderr io.Writer, flag, id string) int {
	target := id
	if i := strings.IndexByte(id, '_'); i >= 0 {
		if task, ok := gedata.GETask(id[i+1:]); ok {
			target = id[:i] + "." + task
		}
	}
	_, errOut, exit, err := runner.Run(context.Background(), "qmod", flag, target)
	if err != nil {
		fmt.Fprintf(stderr, "scancel: error: running qmod: %v\n", err)
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
