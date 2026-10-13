package scontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// slurmTimeLayout is how scontrol prints a timestamp (local time, no zone).
const slurmTimeLayout = "2006-01-02T15:04:05"

// now is the clock RunTime is measured against; tests pin it.
var now = time.Now

// showJob prints the record of a job. Inside a running job the fabricated
// layout is used (rich, allocation-ordered nodelist, REQ-SCT-003); otherwise
// (or for a different job id from a login shell) the job is looked up in GE.
func showJob(runner gedata.Runner, cfg *config.Config, id string, oneliner bool, stdout, stderr io.Writer) int {
	// Prefer the fabricated layout when it is present and describes the requested
	// job (or no id was given: "show my job").
	lay, layErr := loadLayout()
	if layErr == nil && (id == "" || matchesLayout(id, lay)) {
		printRecord(stdout, layoutJobRecord(lay), oneliner)
		return 0
	}
	if id == "" {
		// With no id we can only show "my" job from the in-job layout. A layout
		// that exists but failed to read is a real error, not "not inside a job".
		if layErr != nil && !errors.Is(layErr, os.ErrNotExist) {
			fmt.Fprintf(stderr, "scontrol: error: %v\n", layErr)
			return 1
		}
		fmt.Fprintln(stderr, "scontrol: error: no job id specified (and not inside a job)")
		return 1
	}

	// Look the job up in GE. Array-task ids (4711_2) match the base job number and,
	// when a task is given, that specific task: SLURM index 2 is GE task 3 (same
	// rule as squeue, REQ-SQU-002, SI-14).
	base, task, isTask := strings.Cut(id, "_")
	if isTask {
		var ok bool
		if task, ok = gedata.GETask(task); !ok {
			fmt.Fprintln(stderr, "scontrol: error: Invalid job id specified")
			return 1
		}
	}
	out, errOut, exit, err := runner.Run(context.Background(), "qstat", "-xml", "-u", "*")
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: running qstat: %v\n", err)
		return 1
	}
	if exit != 0 {
		fmt.Fprintf(stderr, "scontrol: error: %s\n", geMessage(nil, errOut, "qstat failed"))
		return 1
	}
	rows, err := gedata.ParseQstatXML(out)
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: error: parsing qstat output: %v\n", err)
		return 1
	}
	for _, r := range rows {
		if r.JobID != base || (task != "" && !gedata.TaskInRange(r.TaskID, task)) {
			continue
		}
		// The listing carries only the master queue instance, so on its own it
		// reports a multi-node job as one node; the detail view gives every
		// granted host (as squeue shows them) and the rest of the record. A
		// failure here is not worth failing the record over: show what the
		// listing knows, with the master host as the one host the job is
		// certainly on, and say on stderr what is missing.
		detail, found, detErr := gedata.FetchJobDetail(context.Background(), runner, base, cfg.GPU.GresComplex)
		if detErr == nil && !found {
			detErr = errors.New("job left Grid Engine during the lookup")
		}
		var d *gedata.JobDetail
		if detErr != nil {
			fmt.Fprintf(stderr, "scontrol: warning: node list unavailable: %v\n", detErr)
		} else {
			d = &detail
		}
		var alloc gedata.JobAllocation
		if d != nil {
			alloc = d.Allocations[r.Key()]
		}
		if len(alloc.Hosts) == 0 && gedata.MapState(r.State) != "PD" {
			fmt.Fprintf(stderr, "scontrol: warning: no granted host list for %s; showing the master host only\n", id)
		}
		printRecord(stdout, geJobRecord(id, task, r, d, alloc, cfg, holdKind(runner, r, d, task, stderr)), oneliner)
		return 0
	}
	fmt.Fprintln(stderr, "scontrol: error: Invalid job id specified")
	return 1
}

// matchesLayout reports whether id (possibly an array-task id) names the job the
// fabricated layout describes.
func matchesLayout(id string, lay *layout.Layout) bool {
	base, _, _ := strings.Cut(id, "_")
	return base == strconv.FormatInt(lay.Job.JobID, 10)
}

func layoutJobRecord(lay *layout.Layout) record {
	hosts := make([]string, len(lay.Nodes))
	for i, n := range lay.Nodes {
		hosts[i] = n.Host
	}
	spares := make([]string, len(lay.Spares))
	for i, n := range lay.Spares {
		spares[i] = n.Host
	}
	return record{
		{{"JobId", strconv.FormatInt(lay.Job.JobID, 10)}, {"JobName", lay.Job.Name}},
		{{"UserId", fmt.Sprintf("%s(%d)", lay.Job.User, lay.Job.UID)}},
		{{"JobState", "RUNNING"}, {"Reason", "None"}},
		{{"Partition", lay.Job.Partition}},
		{{"NodeList", encoders.CompressNodelist(hosts)}},
		{{"NumNodes", strconv.Itoa(len(lay.Nodes))}, {"NumTasks", strconv.Itoa(lay.Tasks.NTasks)}},
		{{"WorkDir", lay.Job.SubmitDir}},
		spareFields(spares, lay.Lost),
	}
}

// spareFields renders a hot-spare job's spares and the nodes they replaced. It
// returns nothing for a job with neither, so every other record is unchanged.
func spareFields(spares, swapped []string) []field {
	if len(spares) == 0 && len(swapped) == 0 {
		return nil
	}
	list := func(hosts []string) string {
		if len(hosts) == 0 {
			return "(null)"
		}
		return encoders.CompressNodelist(hosts)
	}
	return []field{{"SpareNodes", list(spares)}, {"SwappedNodes", list(swapped)}}
}

// geJobRecord renders a job from the qstat listing row r and, when it could be
// read, the job's detail d. task is the requested GE task number ("" for the
// job). Granted hosts are in the scheduler's grant order (never sorted,
// REQ-ENC-002 / SI-41).
func geJobRecord(id, task string, r gedata.JobRow, d *gedata.JobDetail, alloc gedata.JobAllocation, cfg *config.Config,
	hold string) record {
	queue, host, _ := strings.Cut(r.Queue, "@")
	nodelist := encoders.CompressNodelist(alloc.Hosts)
	if nodelist == "" {
		nodelist = host
	}
	if nodelist == "" {
		nodelist = "(null)" // not yet scheduled onto a node
	}
	compact := gedata.MapState(r.State)
	partition := "(null)"
	pe := ""
	if d != nil {
		pe = d.PE
		if queue == "" && len(d.HardQueues) > 0 {
			queue = d.HardQueues[0]
		}
	}
	if p := cfg.PartitionFor(queue, pe); p != "" {
		partition = p
	}
	if d == nil {
		return record{
			{{"JobId", id}, {"JobName", r.Name}},
			{{"UserId", r.User}},
			{{"JobState", gedata.FullState(compact)}, {"Reason", pendingReason(r.State, nil, "", hold)}},
			{{"Partition", partition}},
			{{"NodeList", nodelist}},
			{{"NumCPUs", strconv.Itoa(r.Slots)}, {"NumTasks", strconv.Itoa(r.Slots)}},
		}
	}

	if task == "" {
		task = firstTask(r.TaskID)
	}
	started := compact == "R" || compact == "S" || compact == "CG"
	start := r.Start
	if start.IsZero() {
		start = d.TaskStart(task)
	}
	nodes := len(alloc.Hosts)
	if nodes == 0 && host != "" {
		nodes = 1
	}
	if nodes == 0 && d.PE == "" {
		nodes = 1 // a job without a PE runs on one node
	}
	workDir := d.Cwd
	if workDir == "" {
		workDir = d.Home // GE runs a job without -cwd in the owner's home
	}
	stdout := outputPath(d.Stdout, "o", workDir, *d, task, r.TaskID != "")
	stderr := outputPath(d.Stderr, "e", workDir, *d, task, r.TaskID != "")
	if d.MergeStderr {
		stderr = stdout
	}
	reqTRES := tres(r.Slots, d, cfg, nodes, requestedGPUs(d, cfg))
	allocTRES := "(null)"
	if started {
		allocTRES = tres(r.Slots, d, cfg, nodes, grantedGPUs(d))
	}
	return record{
		{{"JobId", id}, {"JobName", r.Name}},
		{{"UserId", fmt.Sprintf("%s(%s)", d.Owner, d.UID)}, {"GroupId", fmt.Sprintf("%s(%s)", d.Group, d.GID)}},
		{{"JobState", gedata.FullState(compact)}, {"Reason", pendingReason(r.State, d, task, hold)}, {"Dependency", dependency(d)}},
		{{"ExitCode", "0:0"}},
		{{"RunTime", runTime(start, started)}, {"TimeLimit", timeLimit(d)}},
		{{"SubmitTime", slurmTime(d.Submit)}, {"EligibleTime", eligibleTime(d)}},
		{{"StartTime", startTime(start, started)}},
		{{"Partition", partition}},
		{{"NodeList", nodelist}},
		{{"NumNodes", countOrNull(nodes)}, {"NumCPUs", strconv.Itoa(r.Slots)}, {"NumTasks", strconv.Itoa(r.Slots)}},
		{{"ReqTRES", reqTRES}},
		{{"AllocTRES", allocTRES}},
		{{"Command", command(d)}},
		{{"WorkDir", workDir}},
		{{"StdErr", stderr}},
		{{"StdOut", stdout}},
		spareFields(alloc.Spares, alloc.Lost),
	}
}

// firstTask is the GE task a listing row stands for: the task itself, or the
// first of a pending range ("1-10:1" -> "1"); "1" for a job that is no array.
func firstTask(taskID string) string {
	if taskID == "" {
		return "1"
	}
	end := strings.IndexAny(taskID, "-:,")
	if end < 0 {
		return taskID
	}
	return taskID[:end]
}

// pendingReason maps why a pending GE job waits onto SLURM's Reason: a user
// hold (qhold, sbatch -H) is JobHeldUser, a -hold_jid dependency is
// Dependency, any other hold (operator or system) JobHeldAdmin, and a -a start
// time in the future BeginTime. Everything else -- including a running job --
// is None: GE does not say whether a queued job waits for resources or
// priority without scheduler job info, which sites usually turn off. hold is
// the job's hold kind from holdKind, "" when the detail answers alone.
func pendingReason(state string, d *gedata.JobDetail, task, hold string) string {
	if gedata.MapState(state) != "PD" {
		return "None"
	}
	held := strings.ContainsRune(state, 'h')
	switch {
	case d == nil && held:
		return "JobHeldUser"
	case d == nil:
		return "None"
	case d.UserHeld(task) || hold == gedata.HoldUser:
		return "JobHeldUser"
	case len(d.Predecessors) > 0:
		return "Dependency"
	case held:
		return "JobHeldAdmin"
	case d.Begin.After(now()):
		return "BeginTime"
	}
	return "None"
}

// holdKind asks qstat which hold keeps a held job pending when its detail
// cannot say: OCS 9.0 leaves the hold ranges out of `qstat -xml -j`, and a
// detail without a user hold reads the same as one held by an operator (see
// gedata.HoldKinds). "" when the question does not arise or cannot be answered.
func holdKind(runner gedata.Runner, r gedata.JobRow, d *gedata.JobDetail, task string, stderr io.Writer) string {
	if task == "" {
		task = firstTask(r.TaskID) // as geJobRecord reads it
	}
	if d == nil || !strings.ContainsRune(r.State, 'h') || gedata.MapState(r.State) != "PD" ||
		d.UserHeld(task) || len(d.Predecessors) > 0 {
		return ""
	}
	kinds, err := gedata.HoldKinds(context.Background(), runner)
	if err != nil {
		fmt.Fprintf(stderr, "scontrol: warning: hold kind unavailable: %v\n", err)
		return ""
	}
	return kinds[r.JobID]
}

// dependency renders -hold_jid predecessors the way SLURM writes the matching
// dependency: GE starts the job once its predecessors have ended, whatever
// their exit status -- afterany.
func dependency(d *gedata.JobDetail) string {
	if len(d.Predecessors) == 0 {
		return "(null)"
	}
	parts := make([]string, len(d.Predecessors))
	for i, p := range d.Predecessors {
		parts[i] = "afterany:" + p
	}
	return strings.Join(parts, ",")
}

// timeLimit is the job's h_rt, UNLIMITED when it has none.
func timeLimit(d *gedata.JobDetail) string {
	v, ok := d.Resource("h_rt")
	if !ok {
		return "UNLIMITED"
	}
	secs, ok := geSeconds(v)
	if !ok {
		return "UNLIMITED"
	}
	return slurmDuration(secs)
}

// slurmDuration renders seconds as SLURM does: HH:MM:SS, D-HH:MM:SS past a day.
func slurmDuration(secs int) string {
	if secs < 0 {
		secs = 0
	}
	days, h, m, s := secs/86400, (secs%86400)/3600, (secs%3600)/60, secs%60
	if days > 0 {
		return fmt.Sprintf("%d-%02d:%02d:%02d", days, h, m, s)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func runTime(start time.Time, started bool) string {
	if !started || start.IsZero() {
		return "00:00:00"
	}
	return slurmDuration(int(now().Sub(start) / time.Second))
}

func slurmTime(t time.Time) string {
	if t.IsZero() {
		return "Unknown"
	}
	return t.Format(slurmTimeLayout)
}

func startTime(start time.Time, started bool) string {
	if !started {
		return "Unknown"
	}
	return slurmTime(start)
}

// eligibleTime is when the job may start: its -a time, else its submission.
func eligibleTime(d *gedata.JobDetail) string {
	if !d.Begin.IsZero() {
		return slurmTime(d.Begin)
	}
	return slurmTime(d.Submit)
}

func countOrNull(n int) string {
	if n <= 0 {
		return "(null)" // a pending PE job: GE decides the node count at dispatch
	}
	return strconv.Itoa(n)
}

// tres renders a TRES string in SLURM's order: cpu, mem, node, gres/gpu. mem is
// the site's memory complex request, which sbatch makes from --mem, a per-node
// amount; it is multiplied up to the job when the node count is known.
func tres(cpus int, d *gedata.JobDetail, cfg *config.Config, nodes, gpus int) string {
	parts := []string{"cpu=" + strconv.Itoa(cpus)}
	if cfg.MemoryComplex != "" {
		if mem, ok := d.Resource(cfg.MemoryComplex); ok {
			parts = append(parts, "mem="+scaleMemory(mem, nodes))
		}
	}
	if nodes > 0 {
		parts = append(parts, "node="+strconv.Itoa(nodes))
	}
	if gpus > 0 {
		parts = append(parts, "gres/gpu="+strconv.Itoa(gpus))
	}
	return strings.Join(parts, ",")
}

// scaleMemory multiplies an integer memory amount ("2G") by n; any other form
// is shown as requested.
func scaleMemory(v string, n int) string {
	if n <= 1 {
		return v
	}
	digits := strings.TrimRight(v, "KkMmGgTt")
	amount, err := strconv.Atoi(digits)
	if err != nil {
		return v
	}
	return strconv.Itoa(amount*n) + v[len(digits):] // k and K differ in GE: keep the case
}

func requestedGPUs(d *gedata.JobDetail, cfg *config.Config) int {
	if cfg.GPU.GresComplex == "" {
		return 0
	}
	v, _ := d.Resource(cfg.GPU.GresComplex)
	n, _ := strconv.Atoi(v)
	return n
}

func grantedGPUs(d *gedata.JobDetail) int {
	n := 0
	for _, h := range d.GPUs {
		n += len(h.Devices)
	}
	return n
}

// command is the job script (or -b y binary) and its arguments.
func command(d *gedata.JobDetail) string {
	if d.Script == "" {
		return "(null)"
	}
	return strings.Join(append([]string{d.Script}, d.Args...), " ")
}

// outputPath renders a job's -o/-e path as the file GE writes: the pseudo
// variables expanded, a relative path taken from the working directory, and
// GE's default name (<name>.o<job>[.<task>]) for no path or a directory.
func outputPath(p, kind, workDir string, d gedata.JobDetail, task string, isArray bool) string {
	def := d.Name + "." + kind + d.JobID
	taskVar := "undefined"
	if isArray {
		def += "." + task
		taskVar = task
	}
	if p == "" || strings.HasSuffix(p, "/") {
		p += def
	}
	p = strings.NewReplacer(
		"$JOB_ID", d.JobID,
		"$JOB_NAME", d.Name,
		"$USER", d.Owner,
		"$HOME", d.Home,
		"$TASK_ID", taskVar,
	).Replace(p)
	if !path.IsAbs(p) && workDir != "" {
		p = path.Join(workDir, p)
	}
	return p
}
