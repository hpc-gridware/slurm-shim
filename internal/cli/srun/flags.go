// Package srun implements the srun step launcher (spec sec. 7.1-7.4).
package srun

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

// options are the parsed srun invocation.
type options struct {
	req         plan.StepRequest
	command     []string
	label       bool
	output      string
	errorPat    string
	chdir       string
	exportSpec  string
	killFlag    string // "" unset, else "0"/"1" (-K optional value)
	strictFlags bool
	version     bool
	gpuBind     string // --gpu-bind: "" unset, "none", "per_task[:n]"
	testOnly    bool   // --test-only: report the step, launch nothing

	// Interactive-session request (--pty outside an allocation -> qrsh). These are
	// meaningful only on that path; inside an allocation srun ignores them with a
	// warning, matching the fact that a step cannot carry its own allocation.
	pty       bool
	partition string
	jobName   string
	haveTime  bool
	timeSec   int
	mem       string // GE-formatted ("4G"), "" unset
	haveGPUs  bool
	gpus      int
	account   string
	// interactiveFlagsSet records that at least one allocation-shaping flag was
	// given, so the inside-an-allocation path can warn that it ignored them.
	interactiveFlagsSet bool
	// warnings accumulated during parsing (unknown flags, etc.).
	warnings []string
}

// parseFlags parses srun's argv. Everything after the first non-flag token is
// the command, passed through verbatim (REQ-RUN-006). Unknown flags warn and are
// skipped unless strict (REQ-RUN-005). --mpi accepts only "none" (REQ-RUN-004).
func parseFlags(args []string, strict bool, stderr io.Writer) (*options, error) {
	fs := pflag.NewFlagSet("srun", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	// Stop at the first positional so the command and its flags pass through
	// untouched (REQ-RUN-006).
	fs.SetInterspersed(false)
	// Unknown flags are not a parse error; we collect and warn (REQ-RUN-005).
	fs.ParseErrorsAllowlist.UnknownFlags = true

	var (
		nodes        = fs.IntP("nodes", "N", 0, "")
		ntasks       = fs.IntP("ntasks", "n", 0, "")
		tasksPerNode = fs.Int("ntasks-per-node", 0, "")
		cpusPerTask  = fs.IntP("cpus-per-task", "c", 0, "")
		nodelist     = fs.StringP("nodelist", "w", "", "")
		gpusPerTask  = fs.Int("gpus-per-task", 0, "")
		gpuBind      = fs.String("gpu-bind", "", "")
		export       = fs.String("export", "ALL", "")
		output       = fs.StringP("output", "o", "", "")
		errorPat     = fs.StringP("error", "e", "", "")
		label        = fs.BoolP("label", "l", false, "")
		chdir        = fs.String("chdir", "", "")
		chdirD       = fs.StringP("_chdir_d", "D", "", "") // -D alias for --chdir
		mpi          = fs.String("mpi", "", "")
		version      = fs.BoolP("version", "V", false, "")
		kill         = fs.StringP("kill-on-bad-exit", "K", "", "")
		_            = fs.Bool("quiet", false, "")
		_            = fs.CountP("verbose", "v", "")
		// submitit's generated srun line always passes --unbuffered; users may add
		// --cpu-bind via srun_args. Accept both explicitly (no GE behavior to map)
		// so a space-form --cpu-bind value is never mistaken for the command.
		_ = fs.Bool("unbuffered", false, "")
		_ = fs.String("cpu-bind", "", "")
		// SLURM's --test-only: report the step and launch nothing. OR'd with
		// SLURM_SHIM_DRY_RUN so a caller that controls only argv can reach the mode.
		testOnly = fs.Bool("test-only", false, "")

		// Interactive-session flags. Defined (not left unknown) so pflag does not
		// swallow the command as a flag value, and so a real translation exists.
		pty         = fs.Bool("pty", false, "")
		partition   = fs.StringP("partition", "p", "", "")
		timeVal     = fs.StringP("time", "t", "", "")
		memVal      = fs.String("mem", "", "")
		gres        = fs.String("gres", "", "")
		gpus        = fs.String("gpus", "", "")
		gpusPerNode = fs.String("gpus-per-node", "", "")
		account     = fs.StringP("account", "A", "", "")
		qos         = fs.StringP("qos", "q", "", "")
		jobName     = fs.StringP("job-name", "J", "", "")
		// No GE analogue; defined so their values are not read as the command, then
		// warned. --exclusive reuses sbatch's guidance.
		x11       = fs.Bool("x11", false, "")
		overlap   = fs.Bool("overlap", false, "")
		noKill    = fs.Bool("no-kill", false, "")
		exclusive = fs.Bool("exclusive", false, "")
	)
	// -K may be given with no value; default it to "1" when bare.
	fs.Lookup("kill-on-bad-exit").NoOptDefVal = "1"

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if *mpi != "" && *mpi != "none" {
		return nil, fmt.Errorf("srun: error: --mpi=%s is not supported; use native mpirun for MPI", *mpi)
	}

	opt := &options{
		req: plan.StepRequest{
			Nodes:        *nodes,
			NTasks:       *ntasks,
			TasksPerNode: *tasksPerNode,
			CPUsPerTask:  *cpusPerTask,
			GPUsPerTask:  *gpusPerTask,
		},
		command:     fs.Args(),
		label:       *label,
		output:      *output,
		errorPat:    *errorPat,
		chdir:       firstNonEmpty(*chdir, *chdirD),
		exportSpec:  *export,
		killFlag:    *kill,
		strictFlags: strict,
		version:     *version,
		gpuBind:     *gpuBind,
		testOnly:    *testOnly,
		pty:         *pty,
		partition:   *partition,
		jobName:     *jobName,
	}
	if err := applyInteractiveFlags(opt, *timeVal, *memVal, *gres, *gpus, *gpusPerNode, *account, *qos); err != nil {
		return nil, err
	}
	if *x11 {
		opt.warnings = append(opt.warnings, "--x11 is not translated: DISPLAY is forwarded with the environment but xauth is not, so X clients will not connect")
	}
	if *exclusive {
		opt.warnings = append(opt.warnings, "--exclusive is not translated: ask for the node width explicitly (--ntasks-per-node=<cores>) or use a partition sized to the node")
	}
	if *overlap || *noKill {
		opt.warnings = append(opt.warnings, "--overlap/--no-kill are accepted and ignored (no Grid Engine analogue)")
	}
	_ = qos // consumed by applyInteractiveFlags's warning
	if *nodelist != "" {
		hosts, err := encoders.ExpandNodelist(*nodelist)
		if err != nil {
			return nil, fmt.Errorf("srun: error: invalid --nodelist: %w", err)
		}
		opt.req.Nodelist = hosts
	}

	// Surface unknown flags (pflag whitelisted them). strict makes them fatal.
	for _, a := range unknownFlags(args, fs) {
		if strict {
			return nil, fmt.Errorf("srun: error: unrecognized option %q", a)
		}
		opt.warnings = append(opt.warnings, fmt.Sprintf("option %s ignored (slurm-shim)", a))
	}
	return opt, nil
}

// unknownFlags returns the flag-shaped tokens in args (before the first
// positional) that the flag set does not define.
func unknownFlags(args []string, fs *pflag.FlagSet) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" {
			break // first positional; the rest is the command
		}
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if len(a) >= 2 && a[1] != '-' {
			// Short cluster like -lN; check the first rune only.
			name = string(name[0])
			if fs.ShorthandLookup(name) != nil {
				continue
			}
		} else if fs.Lookup(name) != nil {
			continue
		}
		out = append(out, a)
	}
	return out
}

// applyGPUBind resolves how the step binds devices to tasks and folds the result
// into the step request. Precedence follows SLURM: an explicit --gpu-bind wins,
// then SLURM_GPU_BIND (which is how an #SBATCH --gpu-bind directive reaches the
// step), then the site's gpu.bind config. "none" keeps the node's whole grant
// visible to every task (SLURM's default); "per_task[:n]" binds each task to its
// own devices. Unsupported forms (map_gpu, mask_gpu, closest) warn and fall back
// to the default rather than silently pretending to bind.
func applyGPUBind(opt *options, cfg *config.Config, envBind string) {
	spec := strings.TrimSpace(opt.gpuBind)
	if spec == "" {
		spec = strings.TrimSpace(envBind)
	}
	if spec == "" {
		if cfg != nil && cfg.GPU.Bind == "per-task" {
			opt.req.AutoDivideGPUs = true
		}
		return
	}

	// SLURM allows a "verbose," prefix on any form.
	spec = strings.TrimPrefix(spec, "verbose,")
	name, value, _ := strings.Cut(spec, ":")
	opt.req.GPUBindExplicit = true
	switch strings.ToLower(name) {
	case "none":
		opt.req.AutoDivideGPUs = false
		// An explicit "none" is a request NOT to bind, and it has to win over
		// --gpus-per-task: plan.AssignDevices partitions per rank whenever
		// GPUsPerTask > 0, regardless of AutoDivideGPUs, so leaving it set bound
		// each rank anyway -- contradicting SLURM and this repo's own README
		// ("none: the node's whole grant stays visible to every task").
		opt.req.GPUsPerTask = 0
	case "per_task", "per-task":
		opt.req.AutoDivideGPUs = true
		if n, err := strconv.Atoi(value); err == nil && n > 0 && opt.req.GPUsPerTask <= 0 {
			opt.req.GPUsPerTask = n
		}
	default:
		opt.req.GPUBindExplicit = false // unsupported form: fall back to the default
		opt.warnings = append(opt.warnings,
			"--gpu-bind="+spec+" is not supported (slurm-shim); using the default binding")
	}
}

// warnCgroupCannotBind reports a binding request the shim is about to drop:
// under gpu.isolation: cgroup, GE's devices_allow masks per JOB and
// gpuAssignment publishes no per-rank CUDA_VISIBLE_DEVICES (REQ-GPU-003), so
// every task sees the whole grant. Call after applyGPUBind and before the
// warnings are drained.
//
// Only an EXPLICIT request counts. The site default gpu.bind: per-task also sets
// AutoDivideGPUs, and firing on that would warn about GPUs on every step at such
// a site, including CPU-only ones -- this runs before placement, so it cannot
// see whether the step holds any device. An explicit --gpu-bind=none clears both
// fields above, so it is silent here too.
func warnCgroupCannotBind(opt *options, cfg *config.Config) {
	if cfg == nil || cfg.GPU.Isolation != "cgroup" {
		return
	}
	// --gpus-per-task counts on its own; --gpu-bind counts only when it asked to
	// bind (an explicit "none" clears both fields, so it lands here as false).
	requested := opt.req.GPUsPerTask > 0 || (opt.req.GPUBindExplicit && opt.req.AutoDivideGPUs)
	if !requested {
		return
	}
	opt.warnings = append(opt.warnings,
		"per-task GPU binding was requested but gpu.isolation is \"cgroup\": Grid "+
			"Engine masks devices per job, so every task sees the job's whole grant. "+
			"Set gpu.isolation: shim to bind each rank to its own device")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// applyInteractiveFlags parses the value-taking allocation flags (--time, --mem,
// --gres/--gpus, --account, --qos) into opt, using the shared submit converters
// so srun and sbatch agree. --qos has no Grid Engine analogue and is warned.
func applyInteractiveFlags(opt *options, timeVal, memVal, gres, gpus, gpusPerNode, account, qos string) error {
	if account != "" {
		opt.account = account
		opt.interactiveFlagsSet = true
	}
	if qos != "" {
		opt.warnings = append(opt.warnings,
			"--qos is not translated: Grid Engine has no QOS concept")
		opt.interactiveFlagsSet = true
	}
	if timeVal != "" {
		sec, err := submit.ParseSlurmTime(timeVal)
		if err != nil {
			return fmt.Errorf("srun: error: %w", err)
		}
		opt.haveTime, opt.timeSec = true, sec
		opt.interactiveFlagsSet = true
	}
	if memVal != "" {
		opt.mem = submit.ConvertMem(memVal)
		opt.interactiveFlagsSet = true
	}
	// --gres=gpu:N takes precedence over --gpus/--gpus-per-node when both name a
	// count, matching sbatch; any one of them sets the request.
	if n, ok := submit.GresGPUCount(gres); ok {
		opt.haveGPUs, opt.gpus = true, n
		opt.interactiveFlagsSet = true
	} else if v := firstNonEmpty(gpus, gpusPerNode); v != "" {
		n, err := submit.ParseGPUCount(v)
		if err != nil {
			return fmt.Errorf("srun: error: %w", err)
		}
		opt.haveGPUs, opt.gpus = true, n
		opt.interactiveFlagsSet = true
	}
	if opt.partition != "" || opt.pty {
		opt.interactiveFlagsSet = true
	}
	return nil
}
