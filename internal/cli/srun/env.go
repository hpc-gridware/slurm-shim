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

// gpuEnvVar resolves the device-visibility variable this site publishes and the
// variables that must be removed from the rank environment so exactly one device
// mask survives (REQ-GPU-004).
//
// An unrecognised vendor is an error rather than a fallback: silently writing
// CUDA_VISIBLE_DEVICES on an AMD site is precisely the broken behaviour this
// exists to prevent. config.validate only warns, so the refusal happens here,
// where it costs a GPU step rather than every command on the host.
func gpuEnvVar(cfg *config.Config) (write string, drop []string, err error) {
	if cfg == nil {
		return config.GPU{}.DeviceVars()
	}
	return cfg.GPU.DeviceVars()
}

// interactiveDeviceMaskWarning reports a device mask the interactive session is
// about to inherit from the login environment.
//
// An interactive session is launched with qrsh -V, which forwards the caller's
// whole environment, and it never builds a step environment -- so none of the
// per-rank device hygiene applies to it. A CUDA_VISIBLE_DEVICES exported by a
// module file on the login node therefore travels to the compute node and, on
// ROCm, is applied as indices into the granted device list rather than as the
// absolute ids it holds. Warn rather than scrub: the user owns this environment,
// and silently editing what -V forwards would be a surprise of its own.
func interactiveDeviceMaskWarning(cfg *config.Config, haveGPUs bool) string {
	if !haveGPUs {
		return ""
	}
	write, _, err := gpuEnvVar(cfg)
	if err != nil {
		return ""
	}
	var present []string
	for _, name := range []string{
		proto.EnvCUDADevices, proto.EnvROCRDevices,
		proto.EnvHIPDevices, proto.EnvGPUDeviceOrdinal,
	} {
		if name == write {
			continue
		}
		if _, ok := os.LookupEnv(name); ok {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return ""
	}
	return "an interactive session forwards your environment, including " +
		strings.Join(present, ", ") + ", and this site publishes " + write +
		"; a stale mask can select the wrong devices inside the session -- unset it before srun --pty"
}

// gpuRequestWarnings reports the two ways a GPU step can end up with no device
// mask and no error: the request found no devices, or the user asked for a device
// variable this site is about to remove. Both are silent today and both hand the
// job whatever the environment happened to carry.
func (s *supervisor) gpuRequestWarnings(opt *options) []string {
	if opt == nil {
		return nil
	}
	var out []string
	write, drop, err := gpuEnvVar(s.cfg)
	if err != nil {
		return nil
	}

	// A GPU was asked for and none arrived. The usual cause on a first install is
	// gpu.gres_complex naming a complex the cluster does not have, so discovery
	// finds nothing. Without this the step runs on whatever device mask the job
	// environment already carried, against the node's full device list.
	if (opt.haveGPUs || opt.req.GPUsPerTask > 0) && !s.stepHasDevices() {
		msg := "this step requested GPUs but no granted devices were found"
		if s.cfg != nil && s.cfg.GPU.GresComplex != "" {
			msg += " for complex " + strconv.Quote(s.cfg.GPU.GresComplex)
		}
		out = append(out, msg+"; ranks will run with whatever device variables the "+
			"job environment already carries, which may name devices this job was not granted")
	}

	// CUDA_DEVICE_ORDER does not select devices, it decides what a selected index
	// means: with PCI_BUS_ID the indices follow bus address, which is the order
	// nvidia-smi reports and so the order a numeric RSMAP is written in; CUDA's
	// own default, FASTEST_FIRST, ranks by capability instead. On the homogeneous
	// nodes that dominate HPC the two coincide, which is why this is rare rather
	// than theoretical -- but where they differ, the mask the shim wrote names
	// different physical devices than the admin intended.
	//
	// Deliberately a warning and not a removal: dropping it would flip a site that
	// set PCI_BUS_ID precisely to make the ids line up, turning a correct setup
	// into a silently wrong one. The shim cannot know which ordering a site's
	// RSMAP ids were derived from, so it reports rather than decides.
	if write == proto.EnvCUDADevices && s.stepHasDevices() {
		if v, ok := os.LookupEnv(proto.EnvCUDADeviceOrder); ok &&
			!strings.EqualFold(strings.TrimSpace(v), proto.OrderPCIBusID) {
			out = append(out, proto.EnvCUDADeviceOrder+"="+v+
				" changes what an index in "+proto.EnvCUDADevices+" refers to; this "+
				"site's device ids follow nvidia-smi order, so set it to "+
				proto.OrderPCIBusID+" or unset it if your nodes have differing GPUs")
		}
	}

	// An explicit --export of a device variable loses to the vendor rule. SLURM
	// honours --export, so silently discarding the user's most direct statement of
	// intent is the one thing we must not do quietly.
	if s.stepHasDevices() {
		for _, name := range drop {
			if name == write && !s.writesDeviceVar() {
				continue
			}
			if exportNames(opt.exportSpec)[name] {
				out = append(out, "--export names "+name+
					", which is removed because gpu.vendor publishes "+write+
					"; remove it from --export or change gpu.vendor")
			}
		}
	}
	return out
}

// exportNames is the set of variable names assigned explicitly in an --export
// spec ("ALL,FOO=1" -> {FOO}). Bare names forwarded from the environment are not
// included: those are inherited values, not a statement of intent.
func exportNames(spec string) map[string]bool {
	out := map[string]bool{}
	for _, kv := range splitKV(strings.TrimPrefix(strings.TrimSpace(spec), "ALL,")) {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			out[kv[:eq]] = true
		}
	}
	return out
}

// writesDeviceVar reports whether this step will publish a device mask at all.
// Under gpu.isolation: cgroup the shim writes nothing -- GE's devices_allow is
// the backend (REQ-GPU-003) -- so the vendor is never consulted for a write.
func (s *supervisor) writesDeviceVar() bool {
	if s.cfg != nil && s.cfg.GPU.Isolation == "cgroup" {
		return false
	}
	return s.stepHasDevices()
}

// resolveDeviceVars resolves the device-visibility variable this step publishes
// and the ones removed from the rank environment, storing both on the supervisor
// so nothing downstream re-derives them (REQ-GPU-004).
//
// It refuses an unrecognised vendor, but only when the step would actually
// publish a mask. config.validate only warns, because config.Load runs in the PE
// start_proc_args hook where a fatal return reaches every job on the host; the
// refusal belongs here, where it costs one GPU step. It runs once at the srun
// dispatch point, before the dry run and the real launch diverge, so the report
// and the real step cannot disagree.
//
// It is deliberately NOT gated on the raw grant alone: under cgroup isolation the
// vendor changes nothing, and refusing there would fail every GPU step on the
// cluster over a value that is never read. A step that publishes no mask keeps
// the NVIDIA name, which no rank will ever see.
func (s *supervisor) resolveDeviceVars() error {
	write, drop, err := gpuEnvVar(s.cfg)
	if err != nil {
		if s.writesDeviceVar() {
			return err
		}
		s.gpuWrite, s.gpuDrop = proto.EnvCUDADevices, nil
		return nil
	}
	s.gpuWrite, s.gpuDrop = write, drop
	return nil
}

// stepHasDevices reports whether any rank in this step was granted a device.
func (s *supervisor) stepHasDevices() bool {
	for _, r := range s.plan.Ranks {
		if len(r.GPUs) > 0 {
			return true
		}
	}
	return false
}

// dropForeignDeviceVars removes device-visibility variables inherited from the
// job environment -- a site prolog, a module file, a container image, the user's
// own script -- so an inherited value cannot layer on top of the mask the shim
// writes (REQ-GPU-004).
//
// Removal, never assignment to empty: an empty value means "zero devices" to CUDA
// and HIP alike, which is a different bug rather than a fix.
//
// Under gpu.isolation: cgroup the shim writes no mask, but it still removes the
// OTHER vendor's variables. Removal and writing are separate concerns: GE's
// devices_allow makes the runtime enumerate only the granted devices and renumber
// them from zero, so an inherited mask holding absolute ids indexes into that
// filtered list and selects the wrong devices. The variable this site writes is
// left alone there, because under cgroup it is GE's to set.
func (s *supervisor) dropForeignDeviceVars(env []string) []string {
	if !s.stepHasDevices() {
		return env
	}
	return dropEnv(env, s.gpuDrop...)
}

// dropEnv returns env without any KEY=VALUE entry whose key is in keys.
func dropEnv(env []string, keys ...string) []string {
	if len(keys) == 0 {
		return env
	}
	remove := make(map[string]bool, len(keys))
	for _, k := range keys {
		remove[k] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key := kv
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			key = kv[:eq]
		}
		if remove[key] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// gpuAssignment returns the per-rank device list to publish as the vendor device
// variable, or nil under cgroup isolation, where GE's cgroup devices_allow is the
// backend and the shim must not write B12 (REQ-GPU-003).
func gpuAssignment(cfg *config.Config, gpus []string) []string {
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
	return dedupEnv(s.dropForeignDeviceVars(env), s.stepShadows())
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
	envVar := s.gpuWrite
	spec := proto.StepSpec{
		Env:        base,
		Command:    s.opt.command,
		Chdir:      s.opt.chdir,
		Label:      s.opt.label,
		ExportNone: s.opt.exportSpec == "NONE",
		GPUEnvVar:  envVar,
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
			// Under cgroup isolation the shim does not write the device variable;
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
