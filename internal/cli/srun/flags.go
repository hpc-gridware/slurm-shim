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
		_            = fs.StringP("job-name", "J", "", "")
		_            = fs.Bool("quiet", false, "")
		_            = fs.CountP("verbose", "v", "")
		// submitit's generated srun line always passes --unbuffered; users may add
		// --cpu-bind via srun_args. Accept both explicitly (no GE behavior to map)
		// so a space-form --cpu-bind value is never mistaken for the command.
		_ = fs.Bool("unbuffered", false, "")
		_ = fs.String("cpu-bind", "", "")
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
	}
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
