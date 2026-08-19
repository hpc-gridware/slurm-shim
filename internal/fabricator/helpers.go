package fabricator

import (
	"os"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

// defaultEnv falls back to the process environment when no lookup is injected.
func defaultEnv(e Env) Env {
	if e == nil {
		return os.Getenv
	}
	return e
}

// envReader reads the job environment through an injected lookup.
type envReader struct{ get Env }

func (e envReader) int64(k string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(e.get(k)), 10, 64)
	return n
}

func (e envReader) intDefault(k string, def int) int {
	s := strings.TrimSpace(e.get(k))
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// who is the resolved job identity.
type who struct {
	user string
	uid  int
	gid  int
}

func resolveWho(id gedata.Identity) who {
	if id == nil {
		return who{}
	}
	name, uid, gid, _ := id.User()
	return who{user: name, uid: uid, gid: gid}
}

// arrayTaskID returns the numeric SGE_TASK_ID and whether it is a real array
// task. GE emits the literal "undefined" for non-array jobs (REQ-ENV-010).
func arrayTaskID(e envReader) (int64, bool) {
	s := strings.TrimSpace(e.get("SGE_TASK_ID"))
	if s == "" || s == "undefined" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func zeroIf(v int64, has bool) int64 {
	if has {
		return v
	}
	return 0
}

// int64OrDefault reads a numeric env var, returning def when it is unset or
// non-numeric.
func int64OrDefault(e envReader, key string, def int64) int64 {
	s := strings.TrimSpace(e.get(key))
	if s == "" {
		return def
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return def
}

// slurmArray holds the SLURM-facing coordinates of a GE array task.
type slurmArray struct {
	taskID   int64
	min      int64
	max      int64
	step     int64
	count    int64
	hasRange bool
}

// slurmArrayCoords maps a GE array task to its SLURM coordinates. GE task ids are
// 1-based and contiguous over the submitted -t range; SLURM/submitit arrays use
// their own origin/step (0-based for submitit). sbatch --array records the SLURM
// origin/step as SLURM_ARRAY_BASE/STEP job env vars, which this reads to convert
// each GE task back to its SLURM index. When those hints are absent (a native GE
// array), base/step default to the GE range so the mapping is the identity
// SLURM index == SGE_TASK_ID, preserving prior behavior (REQ-ENV-010).
func slurmArrayCoords(e envReader, geTask int64) slurmArray {
	if !isNumeric(strings.TrimSpace(e.get("SGE_TASK_FIRST"))) {
		return slurmArray{taskID: geTask} // no usable GE range metadata
	}
	geFirst := e.int64("SGE_TASK_FIRST")
	geStep := int64OrDefault(e, "SGE_TASK_STEPSIZE", 1)
	if geStep <= 0 {
		geStep = 1
	}
	geLast := e.int64("SGE_TASK_LAST")

	base := int64OrDefault(e, "SLURM_ARRAY_BASE", geFirst)
	step := int64OrDefault(e, "SLURM_ARRAY_STEP", geStep)

	pos := (geTask - geFirst) / geStep // 0, 1, 2, ...
	count := (geLast-geFirst)/geStep + 1
	return slurmArray{
		taskID:   base + pos*step,
		min:      base,
		max:      base + (count-1)*step,
		step:     step,
		count:    count,
		hasRange: true,
	}
}

func uniform(counts []int) bool {
	for i := 1; i < len(counts); i++ {
		if counts[i] != counts[0] {
			return false
		}
	}
	return len(counts) > 0
}

func masterQueue(e envReader, ns nodeSet) string {
	if q := e.get("QUEUE"); q != "" {
		return q
	}
	if len(ns.hosts) > 0 {
		return ns.hosts[0].ClusterQueue
	}
	return ""
}

func partition(e envReader, cfg *config.Config, ns nodeSet) string {
	q := masterQueue(e, ns)
	if alias, ok := cfg.PartitionAliases[q]; ok {
		return alias
	}
	return q
}

// submitDir resolves SLURM_SUBMIT_DIR: the directory sbatch was invoked from
// (SLURM semantics), which GE records as SGE_O_WORKDIR. SGE_CWD_PATH is the
// job's EXECUTION cwd (set by -cwd/-wd) -- with the shim's default -cwd the two
// coincide, but under --chdir they differ and the invocation dir must win, so
// SGE_O_WORKDIR is preferred (verified live: without the preference swap a job
// submitted from a subdir reported SLURM_SUBMIT_DIR=$HOME).
func submitDir(e envReader) string {
	if d := e.get("SGE_O_WORKDIR"); d != "" {
		return d
	}
	return e.get("SGE_CWD_PATH")
}

// unsetPreamble is the fixed set of fabricated SLURM_*/MASTER_* variables the
// emission clears first (REQ-ENV-011), so a child job inheriting a parent's
// environment does not retain phantom values. It excludes the user-input
// control variables SLURM_KILL_BAD_EXIT and all SLURM_SHIM_* (SI-56).
func unsetPreamble() []string {
	return []string{
		"SLURM_JOB_ID", "SLURM_JOBID", "SLURM_JOB_NAME",
		"SLURM_ARRAY_JOB_ID", "SLURM_ARRAY_TASK_ID",
		"SLURM_ARRAY_TASK_MIN", "SLURM_ARRAY_TASK_MAX",
		"SLURM_ARRAY_TASK_STEP", "SLURM_ARRAY_TASK_COUNT",
		// Shim-internal array plumbing (planted by sbatch --array via qsub -v):
		// scrub so a nested job cannot inherit a stale origin/step and shift its
		// own array index. The fabricator reads these from its own env before this
		// preamble is emitted, so clearing them here does not affect the current job.
		"SLURM_ARRAY_BASE", "SLURM_ARRAY_STEP",
		"SLURM_JOB_NUM_NODES", "SLURM_NNODES",
		"SLURM_JOB_NODELIST", "SLURM_NODELIST",
		"SLURM_TASKS_PER_NODE", "SLURM_NTASKS", "SLURM_NPROCS",
		"SLURM_NTASKS_PER_NODE", "SLURM_CPUS_PER_TASK",
		"SLURM_JOB_CPUS_PER_NODE", "SLURM_CPUS_ON_NODE",
		"SLURM_JOB_PARTITION", "SLURM_JOB_ACCOUNT",
		"SLURM_SUBMIT_DIR", "SLURM_SUBMIT_HOST", "SLURM_CLUSTER_NAME",
		"SLURM_JOB_USER", "SLURM_JOB_UID", "SLURM_JOB_GID",
		"SLURM_RESTART_COUNT", "SLURM_DISTRIBUTION", "SLURM_LAUNCH_NODE_IPADDR",
		"SLURM_NODEID", "SLURM_PROCID", "SLURM_LOCALID", "SLURM_GTIDS",
		"SLURM_GPUS_ON_NODE", "SLURM_JOB_GPUS", "SLURM_GPUS_PER_NODE",
		"SLURM_MEM_PER_NODE", "MASTER_ADDR", "MASTER_PORT",
		// Step-level names, cleared so a child does not inherit parent step vars.
		"SLURM_TASK_PID", "SLURMD_NODENAME", "SLURM_STEP_ID", "SLURM_STEPID",
		"SLURM_STEP_NODELIST", "SLURM_STEP_NUM_NODES", "SLURM_STEP_NUM_TASKS",
		"SLURM_STEP_TASKS_PER_NODE",
	}
}

// buildTableA assembles the ordered job-level exports (Table A). Guards follow
// the spec: dual names emitted together, array vars only when numeric, A10/A11
// per their conditions, GPU/mem omitted when absent.
func buildTableA(e envReader, cfg *config.Config, ns nodeSet, geom plan.TaskGeometry, lay *layout.Layout, id who, hasTask bool, taskID int64) []KV {
	var kv []KV
	add := func(k, v string) { kv = append(kv, KV{Key: k, Value: v}) }
	master := lay.Nodes[0]

	jobID := strconv.FormatInt(e.int64("JOB_ID"), 10)
	add("SLURM_JOB_ID", jobID)
	add("SLURM_JOBID", jobID)
	if name := lay.Job.Name; name != "" {
		add("SLURM_JOB_NAME", name)
	}

	if hasTask {
		add("SLURM_ARRAY_JOB_ID", jobID)
		arr := slurmArrayCoords(e, taskID)
		add("SLURM_ARRAY_TASK_ID", strconv.FormatInt(arr.taskID, 10))
		if arr.hasRange {
			add("SLURM_ARRAY_TASK_MIN", strconv.FormatInt(arr.min, 10))
			add("SLURM_ARRAY_TASK_MAX", strconv.FormatInt(arr.max, 10))
			add("SLURM_ARRAY_TASK_STEP", strconv.FormatInt(arr.step, 10))
			add("SLURM_ARRAY_TASK_COUNT", strconv.FormatInt(arr.count, 10))
		}
	}

	nnodes := strconv.Itoa(len(lay.Nodes))
	add("SLURM_JOB_NUM_NODES", nnodes)
	add("SLURM_NNODES", nnodes)

	nodelist := encoders.CompressNodelist(nodeNames(lay))
	add("SLURM_JOB_NODELIST", nodelist)
	add("SLURM_NODELIST", nodelist)

	add("SLURM_TASKS_PER_NODE", encoders.CompressCounts(geom.PerNode))
	ntasks := strconv.Itoa(geom.NTasks)
	add("SLURM_NTASKS", ntasks)
	add("SLURM_NPROCS", ntasks)
	if uniform(geom.PerNode) {
		add("SLURM_NTASKS_PER_NODE", strconv.Itoa(geom.PerNode[0]))
	}

	switch {
	case geom.CPUsPerTaskSet:
		add("SLURM_CPUS_PER_TASK", strconv.Itoa(geom.CPUsPerTask))
	case cfg.EmitCPUsPerTask:
		add("SLURM_CPUS_PER_TASK", "1")
	}

	add("SLURM_JOB_CPUS_PER_NODE", encoders.CompressCounts(rawSlots(lay)))
	add("SLURM_CPUS_ON_NODE", strconv.Itoa(master.Slots))

	if p := lay.Job.Partition; p != "" {
		add("SLURM_JOB_PARTITION", p)
	}
	if a := lay.Job.Account; a != "" {
		add("SLURM_JOB_ACCOUNT", a)
	}
	if d := lay.Job.SubmitDir; d != "" {
		add("SLURM_SUBMIT_DIR", d)
	}
	if h := lay.Job.SubmitHost; h != "" {
		add("SLURM_SUBMIT_HOST", h)
	}
	if c := clusterName(e, cfg); c != "" {
		add("SLURM_CLUSTER_NAME", c)
	}

	if id.user != "" {
		add("SLURM_JOB_USER", id.user)
	}
	add("SLURM_JOB_UID", strconv.Itoa(id.uid))
	add("SLURM_JOB_GID", strconv.Itoa(id.gid))

	if r := strings.TrimSpace(e.get("RESTARTED")); r != "" {
		add("SLURM_RESTART_COUNT", r)
	}
	add("SLURM_DISTRIBUTION", "block")
	if master.IP != "" {
		add("SLURM_LAUNCH_NODE_IPADDR", master.IP)
	}
	add("SLURM_NODEID", "0")
	add("SLURM_PROCID", "0")
	add("SLURM_LOCALID", "0")
	add("SLURM_GTIDS", "0")

	if len(master.GPUs) > 0 {
		add("SLURM_GPUS_ON_NODE", strconv.Itoa(len(master.GPUs)))
		add("SLURM_JOB_GPUS", joinInts(master.GPUs))
		if per, ok := uniformGPUCount(lay); ok {
			add("SLURM_GPUS_PER_NODE", strconv.Itoa(per))
		}
	}

	if lay.Job.MemPerNodeMB > 0 {
		add("SLURM_MEM_PER_NODE", strconv.Itoa(lay.Job.MemPerNodeMB))
	}

	if cfg.ExportMasterAddr {
		add("MASTER_ADDR", lay.Rendezvous.MasterAddr)
		add("MASTER_PORT", strconv.Itoa(lay.Rendezvous.MasterPort))
	}
	return kv
}

func clusterName(e envReader, cfg *config.Config) string {
	if c := e.get("SGE_CELL"); c != "" {
		return c
	}
	return "" // config-provided cluster name is optional
}

func lookup(kv []KV, key string) string {
	for _, e := range kv {
		if e.Key == key {
			return e.Value
		}
	}
	return ""
}

func nodeNames(lay *layout.Layout) []string {
	out := make([]string, len(lay.Nodes))
	for i, n := range lay.Nodes {
		out[i] = n.Host
	}
	return out
}

func rawSlots(lay *layout.Layout) []int {
	out := make([]int, len(lay.Nodes))
	for i, n := range lay.Nodes {
		out[i] = n.Slots
	}
	return out
}

func uniformGPUCount(lay *layout.Layout) (int, bool) {
	if len(lay.Nodes) == 0 {
		return 0, false
	}
	first := len(lay.Nodes[0].GPUs)
	for _, n := range lay.Nodes {
		if len(n.GPUs) != first {
			return 0, false
		}
	}
	return first, true
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
