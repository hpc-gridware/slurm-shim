// Package submit holds the SLURM-flag-to-Grid-Engine translation shared by every
// command that requests an allocation: sbatch (qsub) and srun --pty (qrsh). One
// implementation, consumed by both, so a change to how a partition becomes a
// slot count or how --time becomes h_rt cannot drift between commands
// (todos/011: one resolver, consumed by actor and reporter alike).
//
// Errors carry no command prefix; the caller adds "sbatch: error: " or
// "srun: error: " so its own diagnostics keep their established text.
package submit

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// Request is the geometry and resource part of a submission, in SLURM terms.
// Every field is what the user asked for; Have* records whether the flag was
// given at all, since zero is a legal value for several of them.
type Request struct {
	Partition string

	Nodes         int
	NTasks        int
	NTasksPerNode int
	CPUsPerTask   int
	HaveNTasks    bool
	HavePerNode   bool

	HaveTime    bool
	TimeSec     int
	HaveSignal  bool
	SignalDelay int // seconds before the time limit to deliver --signal

	Mem string // GE-formatted memory value ("4G"), "" if unset

	HaveGPUs        bool
	GPUs            int
	HaveGPUsPerTask bool
	GPUsPerTask     int

	ExportSpec string // --export value; "" means the SLURM default ALL
}

// ResolveGeometry derives the task count and cpus-per-task SLURM would use from
// the flags given, with SLURM's defaults for the ones that were not.
func (r Request) ResolveGeometry() (ntasks, cpusPerTask int) {
	cpusPerTask = r.CPUsPerTask
	if cpusPerTask < 1 {
		cpusPerTask = 1
	}
	switch {
	case r.HaveNTasks:
		ntasks = r.NTasks
	case r.Nodes > 0 && r.HavePerNode:
		ntasks = r.Nodes * r.NTasksPerNode
	case r.Nodes > 0:
		ntasks = r.Nodes
	case r.HavePerNode:
		ntasks = r.NTasksPerNode
	default:
		ntasks = 1
	}
	if ntasks < 1 {
		ntasks = 1
	}
	return ntasks, cpusPerTask
}

// TasksPerNode is how many tasks land on one node of the requested layout.
func (r Request) TasksPerNode() int { return r.TasksOnNode(r.Nodes) }

// TasksOnNode is how many TASKS land on one node when the job occupies nodeCount
// of them. Both the emitted `-l <gres>` request and the dry run's per-node device
// count go through here so they cannot drift apart.
//
// The unit matters: with an allocation rule in play it is tempting to reuse the
// -par value, but -par counts SLOTS per node, and slots are tasks x cpus-per-task.
// `-N 3 --ntasks-per-node=2 -c 4 --gpus-per-task=1` pins -par 8 while only 2 tasks
// land on each node -- deriving devices from 8 would request four times the GPUs
// the job can use, on a host that likely has none to spare.
func (r Request) TasksOnNode(nodeCount int) int {
	if r.HavePerNode && r.NTasksPerNode > 0 {
		return r.NTasksPerNode
	}
	if nodeCount < 1 {
		nodeCount = 1
	}
	ntasks, _ := r.ResolveGeometry()
	if perNode := (ntasks + nodeCount - 1) / nodeCount; perNode > 0 {
		return perNode
	}
	return 1
}

// Slots turns a partition's slots rule and the request's geometry into the PE
// slot count to ask Grid Engine for.
func Slots(r Request, part config.Partition) (int, error) {
	n, perTask, err := config.ParseSlotsRule(part.Slots)
	if err != nil {
		// The rule also warns at config load, but the failure belongs here: only the
		// submission that actually names this partition is affected.
		return 0, fmt.Errorf("partition %q: %w", r.Partition, err)
	}
	if perTask {
		ntasks, cpt := r.ResolveGeometry()
		return ntasks * cpt, nil
	}
	return n, nil
}

// ResourceList renders the `-l` value: h_rt (and s_rt when --signal asks for
// warning time), the site's memory complex, and the site's GPU complex.
func ResourceList(cfg *config.Config, r Request) string {
	var l []string
	if r.HaveTime {
		l = append(l, "h_rt="+strconv.Itoa(r.TimeSec))
		if r.HaveSignal && r.SignalDelay > 0 && r.SignalDelay < r.TimeSec {
			l = append(l, "s_rt="+strconv.Itoa(r.TimeSec-r.SignalDelay))
		}
	}
	if r.Mem != "" && cfg.MemoryComplex != "" {
		l = append(l, cfg.MemoryComplex+"="+r.Mem)
	}
	if n, ok := GPURequest(r); ok && cfg.GPU.GresComplex != "" {
		l = append(l, cfg.GPU.GresComplex+"="+strconv.Itoa(n))
	}
	return strings.Join(l, ",")
}

// GPURequest is the node-level GPU count the request implies, if any.
func GPURequest(r Request) (int, bool) {
	if r.HaveGPUs {
		return r.GPUs, true
	}
	if r.HaveGPUsPerTask {
		return r.GPUsPerTask * r.TasksPerNode(), true
	}
	return 0, false
}

// AllocationRule is the `-par` value that pins the requested layout, or a warning
// explaining why the layout cannot be pinned. Emit reports whether there is a
// value to pass.
type AllocationRule struct {
	Value string
	Nodes int
	Warn  string
}

// Emit reports whether the rule carries a -par value.
func (a AllocationRule) Emit() bool { return a.Value != "" }

// ParSpec derives the allocation rule that pins the requested layout on a
// per-task partition, or explains why it cannot.
func ParSpec(r Request, part config.Partition, slots int) AllocationRule {
	// A literal `slots:` rule is the site declaring that geometry does not size this
	// partition -- Slots discards --ntasks entirely for it. Dividing that
	// site-chosen total by a user-chosen --nodes fabricates a per-node width neither
	// party stated, and -w e then turns it into a hard refusal for a job that ran
	// before. Verified: `-p batch -N 1 -n 4` on `slots: "16"` emitted
	// `-par $pe_slots` and was refused on 14-slot hosts, with the user's --ntasks
	// invisible in both the request and the diagnostic.
	if _, perTask, err := config.ParseSlotsRule(part.Slots); err != nil || !perTask {
		if r.Nodes >= 1 || (r.HavePerNode && r.NTasksPerNode >= 1) {
			return AllocationRule{Warn: PinnedSlotsWarning(r.Partition, slots)}
		}
		return AllocationRule{}
	}
	ntasks, _ := r.ResolveGeometry()
	switch {
	// The `>= 1` tests come first and are not redundant: geometry validation
	// rejects negatives and absurd values but accepts zero, so `--nodes=0` and
	// `--ntasks-per-node=0` reach here and would otherwise divide by zero.
	case r.Nodes >= 1:
		return RuleFor(ntasks, r.Nodes, slots)
	case r.HavePerNode && r.NTasksPerNode >= 1:
		// SLURM would spread a remainder (7 tasks at 2 per node -> 2,2,2,1). Grid
		// Engine grants the same count on every host under a fixed rule, so there
		// is no faithful value to emit.
		if ntasks%r.NTasksPerNode != 0 {
			return AllocationRule{Warn: UnevenSpreadWarning(fmt.Sprintf(
				"%d task(s) at %d per node", ntasks, r.NTasksPerNode), slots)}
		}
		return RuleFor(ntasks, ntasks/r.NTasksPerNode, slots)
	default:
		// No layout was stated, so there is nothing to honor -- the site's PE keeps
		// deciding, exactly as before.
		return AllocationRule{}
	}
}

// RuleFor is the -par value for ntasks over nodes when the total is slots.
func RuleFor(ntasks, nodes, slots int) AllocationRule {
	if slots%nodes != 0 {
		return AllocationRule{Warn: UnevenSpreadWarning(fmt.Sprintf(
			"%d task(s) over %d node(s)", ntasks, nodes), slots)}
	}
	par := slots / nodes
	if par < 1 {
		return AllocationRule{}
	}
	if nodes == 1 {
		// Identical placement to `-par <slots>`, but it survives a PE slot range
		// and reads as intent rather than arithmetic in `qstat -j`.
		return AllocationRule{Value: "$pe_slots", Nodes: 1}
	}
	return AllocationRule{Value: strconv.Itoa(par), Nodes: nodes}
}

// PinnedSlotsWarning explains a layout request on a partition whose slots rule
// is a literal count.
func PinnedSlotsWarning(partition string, slots int) string {
	return fmt.Sprintf(
		"the requested layout is not enforced on partition %q: its slots rule pins every "+
			"request at %d slot(s) regardless of geometry, so --nodes/--ntasks-per-node "+
			"cannot size it and the PE's allocation_rule places the nodes. Submit to a "+
			"\"per-task\" partition to pin the layout", partition, slots)
}

// UnevenSpreadWarning explains a layout Grid Engine cannot grant evenly.
func UnevenSpreadWarning(layout string, slots int) string {
	return fmt.Sprintf(
		"the requested layout (%s) is uneven; Grid Engine grants the same number of "+
			"slots on every node, so the node count is left to the PE's allocation_rule "+
			"and %d slot(s) may land anywhere. Use a task count that divides by the node "+
			"count to pin the layout", layout, slots)
}

// ExportArgs maps SLURM's --export value to qsub/qrsh -V / -v arguments. The
// SLURM default (empty or ALL) forwards the whole submit environment.
func ExportArgs(spec string) []string {
	spec = strings.TrimSpace(spec)
	upper := strings.ToUpper(spec)
	switch upper {
	case "", "ALL":
		return []string{"-V"}
	case "NONE", "NIL":
		return nil
	}
	var args []string
	rest := spec
	if strings.HasPrefix(upper, "ALL,") {
		args = append(args, "-V")
		rest = spec[len("ALL,"):]
	}
	for _, kv := range strings.Split(rest, ",") {
		if kv = strings.TrimSpace(kv); kv != "" {
			args = append(args, "-v", kv)
		}
	}
	return args
}

// ParseSlurmTime converts a SLURM time limit (M, M:S, H:M:S, D-H, D-H:M,
// D-H:M:S) to seconds.
func ParseSlurmTime(val string) (int, error) {
	val = strings.TrimSpace(val)
	days := 0
	hasDays := false
	if d := strings.IndexByte(val, '-'); d >= 0 {
		n, err := strconv.Atoi(val[:d])
		if err != nil {
			return 0, fmt.Errorf("--time: invalid days in %q", val)
		}
		days, val, hasDays = n, val[d+1:], true
	}
	parts := strings.Split(val, ":")
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return 0, fmt.Errorf("--time: invalid value %q", val)
		}
		nums[i] = n
	}
	var h, m, s int
	switch {
	case hasDays: // D-HH[:MM[:SS]] -- keyed on the dash, not days>0, so "0-12:30" works
		h = nums[0]
		if len(nums) > 1 {
			m = nums[1]
		}
		if len(nums) > 2 {
			s = nums[2]
		}
	case len(nums) == 1: // minutes
		m = nums[0]
	case len(nums) == 2: // MM:SS
		m, s = nums[0], nums[1]
	default: // HH:MM:SS
		h, m, s = nums[0], nums[1], nums[2]
	}
	return days*86400 + h*3600 + m*60 + s, nil
}

// ConvertMem turns a SLURM memory value ("4GB", "512", "2G") into Grid Engine's
// form ("4G", "512M", "2G"). A bare number is megabytes, SLURM's default unit.
func ConvertMem(val string) string {
	v := strings.TrimSpace(val)
	if v == "" {
		return ""
	}
	up := strings.ToUpper(v)
	for _, suf := range []struct{ slurm, ge string }{
		{"KB", "K"}, {"MB", "M"}, {"GB", "G"}, {"TB", "T"},
	} {
		if strings.HasSuffix(up, suf.slurm) {
			return strings.TrimSpace(up[:len(up)-2]) + suf.ge
		}
	}
	last := up[len(up)-1]
	if last == 'K' || last == 'M' || last == 'G' || last == 'T' {
		return up
	}
	return up + "M" // bare number: SLURM default unit is MB
}

// ParseGPUCount reads --gpus / --gpus-per-node, which may be "N" or "type:N".
func ParseGPUCount(val string) (int, error) {
	v := val
	if c := strings.LastIndexByte(v, ':'); c >= 0 {
		v = v[c+1:]
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--gpus: invalid count %q", val)
	}
	return n, nil
}

// GresGPUCount reads the gpu entry of a --gres list ("gpu:2", "gpu:a100:1",
// "gpu"). ok is false when the list names no GPU.
func GresGPUCount(val string) (int, bool) {
	for _, entry := range strings.Split(val, ",") {
		entry = strings.TrimSpace(entry)
		if !strings.HasPrefix(entry, "gpu:") && entry != "gpu" {
			continue
		}
		fields := strings.Split(entry, ":")
		if n, err := strconv.Atoi(fields[len(fields)-1]); err == nil && n >= 0 {
			return n, true
		}
		return 1, true // "gpu" or "gpu:type" without a count means one
	}
	return 0, false
}
