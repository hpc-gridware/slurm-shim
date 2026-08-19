package srun

import (
	"os"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/mux"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// gpuAssignment returns the per-rank device list to publish as
// CUDA_VISIBLE_DEVICES, or nil under cgroup isolation, where GE's cgroup
// devices_allow is the backend and the shim must not write B12 (REQ-GPU-003).
func gpuAssignment(cfg *config.Config, gpus []int) []int {
	if cfg != nil && cfg.GPU.Isolation == "cgroup" {
		return nil
	}
	return gpus
}

func uniform(counts []int) bool {
	for i := 1; i < len(counts); i++ {
		if counts[i] != counts[0] {
			return false
		}
	}
	return len(counts) > 0
}

// baseEnv builds the environment shared by every rank in the step: the job
// environment filtered per --export, with the step-level shadow variables
// overlaid (SLURM_NTASKS etc. shadowed to the step geometry, REQ-ENV-041).
func (s *supervisor) baseEnv() []string {
	spec := s.opt.exportSpec
	var env []string
	switch {
	case spec == "" || spec == "ALL":
		env = os.Environ()
	case strings.HasPrefix(spec, "ALL,"):
		env = append(os.Environ(), splitKV(spec[len("ALL,"):])...)
	case spec == "NONE":
		env = minimalEnv()
	default:
		env = append(minimalEnv(), splitKV(spec)...)
	}
	return dedupEnv(env, s.stepShadows())
}

// stepShadows are the step-scoped SLURM_* values that shadow the job-level ones.
func (s *supervisor) stepShadows() []string {
	counts := s.stepPerNode()
	ntasks := strconv.Itoa(s.plan.NTasks)
	shadows := []string{
		"SLURM_NTASKS=" + ntasks,
		"SLURM_NPROCS=" + ntasks,
		"SLURM_TASKS_PER_NODE=" + encoders.CompressCounts(counts),
		"SLURM_CPUS_PER_TASK=" + strconv.Itoa(s.plan.CPUsPerTask),
		"SLURM_DISTRIBUTION=block",
		"SLURM_STEP_ID=" + strconv.Itoa(s.stepID),
		"SLURM_STEPID=" + strconv.Itoa(s.stepID),
		"SLURM_STEP_NODELIST=" + encoders.CompressNodelist(joinHosts(s.plan.Nodes)),
		"SLURM_STEP_NUM_NODES=" + strconv.Itoa(len(s.plan.Nodes)),
		"SLURM_STEP_NUM_TASKS=" + ntasks,
		"SLURM_STEP_TASKS_PER_NODE=" + encoders.CompressCounts(counts),
	}
	if uniform(counts) {
		shadows = append(shadows, "SLURM_NTASKS_PER_NODE="+strconv.Itoa(counts[0]))
	}
	if len(s.lay.Nodes) > 0 && s.lay.Nodes[0].IP != "" {
		shadows = append(shadows, "SLURM_LAUNCH_NODE_IPADDR="+s.lay.Nodes[0].IP)
	}
	return shadows
}

// stepSpec builds the StepSpec delivered to the stepper on step node ni.
func (s *supervisor) stepSpec(base []string, ni int) proto.StepSpec {
	node := s.plan.Nodes[ni]
	gtids := s.hostGTIDs(ni)
	spec := proto.StepSpec{
		Env:        base,
		Command:    s.opt.command,
		Chdir:      s.opt.chdir,
		Label:      s.opt.label,
		ExportNone: s.opt.exportSpec == "NONE",
	}
	for _, r := range s.plan.Ranks {
		if r.StepNodeIndex != ni {
			continue
		}
		rs := proto.RankSpec{
			Rank:   r.Rank,
			Local:  r.Local,
			NodeID: r.StepNodeIndex,
			Cpuset: r.Cpuset,
			// Under cgroup isolation the shim does not write CUDA_VISIBLE_DEVICES;
			// OCS cgroup devices_allow already restricts the visible GPUs and the
			// granted env passes through untouched (REQ-GPU-003).
			GPUs: gpuAssignment(s.cfg, r.GPUs),
			EnvDelta: []string{
				"SLURM_PROCID=" + strconv.Itoa(r.Rank),
				"SLURM_LOCALID=" + strconv.Itoa(r.Local),
				"SLURM_NODEID=" + strconv.Itoa(r.StepNodeIndex),
				"SLURM_GTIDS=" + gtids,
				"SLURM_CPUS_ON_NODE=" + strconv.Itoa(node.Slots),
			},
		}
		rs.StdoutFile = s.outputPath(s.opt.output, r, node)
		rs.StderrFile = s.outputPath(s.opt.errorPat, r, node)
		spec.Ranks = append(spec.Ranks, rs)
	}
	return spec
}

// hostGTIDs is the comma-joined list of global rank ids on step node ni (B-GTIDS).
func (s *supervisor) hostGTIDs(ni int) string {
	var ids []string
	for _, r := range s.plan.Ranks {
		if r.StepNodeIndex == ni {
			ids = append(ids, strconv.Itoa(r.Rank))
		}
	}
	return strings.Join(ids, ",")
}

// outputPath expands an output pattern for a rank, or "" to stream (no pattern).
// The array coordinates (%A/%a) are the 0-based SLURM values resolved once on the
// supervisor from the fabricated job env - the same values submitit's job uses to
// build its result/log paths - so srun-written logs land exactly where submitit
// reads them. They default to the plain job id / 0 for a non-array job.
func (s *supervisor) outputPath(pattern string, r plan.PlacedRank, node plan.StepNode) string {
	if pattern == "" {
		return ""
	}
	return mux.ExpandPattern(pattern, mux.PatternFields{
		JobID:       s.lay.Job.JobID,
		ArrayJobID:  s.arrayJobID,
		ArrayTaskID: s.arrayTaskID,
		StepID:      s.stepID,
		Rank:        r.Rank,
		NodeID:      r.StepNodeIndex,
		NodeName:    node.Host,
		JobName:     s.lay.Job.Name,
		User:        s.user,
	})
}

func minimalEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		switch {
		case key == "HOME", key == "USER", key == "PATH", key == "TMPDIR",
			strings.HasPrefix(key, "SLURM_"):
			out = append(out, kv)
		}
	}
	return out
}

func splitKV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func dedupEnv(base, overlay []string) []string {
	idx := map[string]int{}
	var out []string
	put := func(kv string) {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if j, ok := idx[key]; ok {
			out[j] = kv
			return
		}
		idx[key] = len(out)
		out = append(out, kv)
	}
	for _, kv := range base {
		put(kv)
	}
	for _, kv := range overlay {
		put(kv)
	}
	return out
}
