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

// Package srun implements the srun step launcher (spec sec. 7.1-7.4).
package srun

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
	"github.com/hpc-gridware/slurm-shim/internal/slurmopts"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

// options are the parsed srun invocation.
type options struct {
	// elastic is --x-elastic (on, off, auto, or "" for the job's/site's
	// default): whether this step replaces a lost node with a hot spare.
	elastic string

	req        plan.StepRequest
	command    []string
	label      bool
	output     string
	errorPat   string
	chdir      string
	exportSpec string
	killFlag   string // "" unset, else "0"/"1" (-K optional value)
	cpuBind    string // --cpu-bind as given
	// bindMode is the step's CPU binding mode (cpubind.Mode*), from --cpu-bind
	// or SLURM_CPU_BIND; see applyCPUBind.
	bindMode string
	// externalLauncher is --external-launcher: the step starts the daemons of
	// an MPI launcher (OpenMPI's mpirun runs `srun --external-launcher ...
	// prted`). See applyExternalLauncher.
	externalLauncher bool
	// jobID is --jobid as given; only this job's own id is accepted.
	jobID string
	// distribution is --distribution when it has an effect ("arbitrary").
	distribution string
	// mpi is the MPI type (config.MPINone, config.MPIPMIx, "" unset): from
	// --mpi, and after Run resolves it, from SLURM_MPI_TYPE or mpi.default.
	mpi         string
	strictFlags bool
	version     bool
	gpuBind     string // --gpu-bind: "" unset, "none", "per_task[:n]"
	testOnly    bool   // --test-only: report the step, launch nothing
	// immediate is -I/--immediate ("" unset, "set" bare, else seconds): start
	// the allocation at once or fail. input is -i/--input ("none" closes stdin).
	immediate string
	input     string

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
	// Cluster resources for a new allocation (srun or salloc outside one):
	// licenses, features, a whole node, hosts to avoid, local disk, and the
	// per-unit memory kinds (memKind "mem-per-cpu"/"mem-per-gpu", else "").
	licenses   string
	constraint string
	exclusive  string // "" unset, "set" bare, else the value
	exclude    []string
	tmp        string
	memKind    string
	// allocationOnly names the given flags that only shape a new allocation
	// (-L, -C, --tmp, --mem-per-cpu, --mem-per-gpu): a step inside one cannot
	// honour them, which strict_flags refuses.
	allocationOnly []string
	// untranslated lists the SLURM options given that the shim does not
	// translate (each also warned), for a caller that must refuse one.
	untranslated []string
	// warnings accumulated during parsing (unknown flags, etc.).
	warnings []string
}

// parseFlags parses srun's argv. Everything after the first non-flag token is
// the command, passed through verbatim (REQ-RUN-006). Unknown flags warn and are
// skipped unless strict (REQ-RUN-005). --mpi takes none or pmix (REQ-RUN-004, normalizeMPI).
func parseFlags(args []string, strict bool, stderr io.Writer) (*options, error) {
	return parseFlagsWith(slurmopts.For("srun"), args, strict, stderr)
}

// parseFlagsWith is parseFlags with another command's SLURM option table, for
// salloc, which shares srun's allocation options.
func parseFlagsWith(table slurmopts.Table, args []string, strict bool, stderr io.Writer) (*options, error) {
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
		// --cpu-bind via srun_args. Defined explicitly so a space-form --cpu-bind
		// value is never mistaken for the command (applyCPUBind).
		_       = fs.Bool("unbuffered", false, "")
		cpuBind = fs.String("cpu-bind", "", "")
		// What OpenMPI's mpirun passes when it starts its daemons through srun
		// (PRRTE plm/slurm): --external-launcher, --distribution=arbitrary,
		// --jobid. Defined so they are handled, not warned as untranslated.
		externalLauncher = fs.Bool("external-launcher", false, "")
		distribution     = fs.StringP("distribution", "m", "", "")
		jobID            = fs.String("jobid", "", "")
		// SLURM's --test-only: report the step and launch nothing. OR'd with
		// SLURM_SHIM_DRY_RUN so a caller that controls only argv can reach the mode.
		testOnly = fs.Bool("test-only", false, "")

		// Interactive-session flags. Defined (not left unknown) so pflag does not
		// swallow the command as a flag value, and so a real translation exists.
		pty         = fs.Bool("pty", false, "")
		partition   = fs.StringP("partition", "p", "", "")
		timeVal     = fs.StringP("time", "t", "", "")
		gres        = fs.String("gres", "", "")
		gpus        = fs.String("gpus", "", "")
		gpusPerNode = fs.String("gpus-per-node", "", "")
		account     = fs.StringP("account", "A", "", "")
		qos         = fs.StringP("qos", "q", "", "")
		jobName     = fs.StringP("job-name", "J", "", "")
		// No GE analogue; defined so their values are not read as the command, then
		// warned. --exclusive reuses sbatch's guidance.
		// --x11, --no-kill and --exclusive take an optional value in SLURM
		// (--x11=all, --exclusive=user): strings with a bare-form default.
		// -I and -i shape the session srun or salloc starts outside an
		// allocation (qrsh -now yes, stdin).
		immediate = fs.StringP("immediate", "I", "", "")
		input     = fs.StringP("input", "i", "", "")
		x11       = fs.String("x11", "", "")
		overlap   = fs.Bool("overlap", false, "")
		noKill    = fs.StringP("no-kill", "k", "", "")
		exclusive = fs.String("exclusive", "", "")
		// Allocation resources, translated for a new allocation (Phase 1b).
		licenses   = fs.StringP("licenses", "L", "", "")
		constraint = fs.StringP("constraint", "C", "", "")
		exclude    = fs.StringP("exclude", "x", "", "")
		tmp        = fs.String("tmp", "", "")
		// Hot spares: --x-elastic picks whether this step replaces a lost node
		// with a spare. --x-spares belongs to sbatch, which grants the spares.
		elastic = fs.String("x-elastic", "", "")
		spares  = fs.String("x-spares", "", "")
	)
	// --mem, --mem-per-cpu and --mem-per-gpu share one value: SLURM takes one
	// of the three, and the last given wins (SALLOC_MEM_PER_CPU, prepended,
	// loses to a --mem on the command line).
	var mem memChoice
	for _, kind := range []string{"mem", "mem-per-cpu", "mem-per-gpu"} {
		fs.Var(memFlag{&mem, kind}, kind, "")
	}
	// -K may be given with no value; default it to "1" when bare.
	fs.Lookup("kill-on-bad-exit").NoOptDefVal = "1"
	for _, n := range []string{"x11", "no-kill", "exclusive", "immediate"} {
		fs.Lookup(n).NoOptDefVal = "set"
	}
	// Every other SLURM option of the command, with SLURM's arity, so its value
	// is never read as the command (REQ-RUN-005). Used ones warn below.
	untranslated := defineSlurmOptions(fs, table)

	if err := fs.Parse(longForms(args, fs, table)); err != nil {
		return nil, err
	}

	if *spares != "" {
		return nil, fmt.Errorf("srun: error: --x-spares is granted per job: pass it to sbatch (or a #SHIM line); " +
			"srun --x-elastic picks whether a step uses them")
	}
	switch strings.ToLower(*elastic) {
	case "", config.ElasticAuto, config.ElasticOn, config.ElasticOff:
	default:
		return nil, fmt.Errorf("srun: error: --x-elastic=%q: expected on, off or auto", *elastic)
	}
	mpiType, err := normalizeMPI(*mpi, "--mpi")
	if err != nil {
		return nil, err
	}

	opt := &options{
		req: plan.StepRequest{
			Nodes:        *nodes,
			NTasks:       *ntasks,
			TasksPerNode: *tasksPerNode,
			CPUsPerTask:  *cpusPerTask,
			GPUsPerTask:  *gpusPerTask,
		},
		command:          fs.Args(),
		label:            *label,
		output:           *output,
		errorPat:         *errorPat,
		chdir:            firstNonEmpty(*chdir, *chdirD),
		exportSpec:       *export,
		killFlag:         *kill,
		cpuBind:          *cpuBind,
		jobID:            *jobID,
		mpi:              mpiType,
		strictFlags:      strict,
		externalLauncher: *externalLauncher,
		version:          *version,
		gpuBind:          *gpuBind,
		testOnly:         *testOnly,
		pty:              *pty,
		immediate:        *immediate,
		input:            *input,
		elastic:          strings.ToLower(*elastic),
		partition:        *partition,
		jobName:          *jobName,
	}
	if err := applyInteractiveFlags(opt, *timeVal, *gres, *gpus, *gpusPerNode, *account, *qos); err != nil {
		return nil, err
	}
	if *x11 != "" {
		opt.warnings = append(opt.warnings, "--x11 is not translated: DISPLAY is forwarded with the environment but xauth is not, so X clients will not connect")
	}
	if err := applyAllocationResources(opt, *licenses, *constraint, *exclusive, *exclude, *tmp, mem); err != nil {
		return nil, err
	}
	// --overlap is what every step here already does: steps share the
	// allocation's resources. Nothing to translate, nothing to warn about.
	_ = overlap
	// An external launcher passes --no-kill on purpose: it decides what a lost
	// daemon means. Saying "ignored" to it is noise.
	if *noKill != "" && !*externalLauncher {
		opt.warnings = append(opt.warnings, "--no-kill is accepted and ignored (no Grid Engine analogue)")
	}
	_ = qos // consumed by applyInteractiveFlags's warning
	hosts, err := expandNodelist(*nodelist)
	if err != nil {
		return nil, fmt.Errorf("srun: error: %w", err)
	}
	opt.req.Nodelist = hosts
	if err := applyDistribution(opt, *distribution, strict); err != nil {
		return nil, err
	}

	// SLURM options the shim does not translate: parsed with their arity, then
	// warned. strict makes them fatal.
	var used []string
	fs.Visit(func(f *pflag.Flag) {
		if untranslated[f.Name] {
			used = append(used, "--"+f.Name)
		}
	})
	opt.untranslated = used
	for _, name := range used {
		if strict {
			return nil, fmt.Errorf("srun: error: option %s is not supported by slurm-shim", name)
		}
		opt.warnings = append(opt.warnings, fmt.Sprintf("option %s ignored (slurm-shim)", name))
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

// defineSlurmOptions defines on fs every option of SLURM's table that fs does
// not define yet, with SLURM's arity: a value option as a string, an
// optional-value option as a string with a bare-form default, the rest as
// booleans. A short spelling is added where it is still free. It returns the
// long names it defined, which the shim does not translate.
func defineSlurmOptions(fs *pflag.FlagSet, table slurmopts.Table) map[string]bool {
	defined := map[string]bool{}
	for _, o := range table.Options() {
		long, short := "", ""
		for _, n := range o.Names {
			switch {
			case strings.HasPrefix(n, "--"):
				if long == "" {
					long = n[2:]
				}
			case len(n) == 2:
				short = n[1:]
			}
		}
		if long == "" || fs.Lookup(long) != nil {
			continue
		}
		if short != "" && fs.ShorthandLookup(short) != nil {
			short = ""
		}
		switch o.Value {
		case slurmopts.None:
			fs.BoolP(long, short, false, "")
		default:
			fs.StringP(long, short, "", "")
			if o.Value == slurmopts.Optional {
				fs.Lookup(long).NoOptDefVal = "set"
			}
		}
		fs.Lookup(long).Hidden = true
		defined[long] = true
	}
	return defined
}

// longForms rewrites SLURM short options whose long form fs defines without
// that shorthand (-G for --gpus, -u for --unbuffered) to the long form, so
// pflag parses them with the right arity instead of swallowing the next token.
// It stops at the first positional (the command) and steps over each option's
// value on the way.
func longForms(args []string, fs *pflag.FlagSet, table slurmopts.Table) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || a == "-" || !strings.HasPrefix(a, "-") {
			return append(out, args[i:]...)
		}
		var f *pflag.Flag
		if strings.HasPrefix(a, "--") {
			name, _, _ := strings.Cut(a[2:], "=")
			f = fs.Lookup(name)
		} else {
			f = fs.ShorthandLookup(a[1:2])
			o, ok := table.Lookup(a)
			long := ""
			if ok {
				long = longName(o)
			}
			switch {
			case long == "" || fs.Lookup(long) == nil:
			case len(a) > 2 && o.Value != slurmopts.None:
				// -I60, -G2: pflag would read an optional value's digits as
				// more shorthands, so pass every attached value in long form.
				f, a = fs.Lookup(long), "--"+long+"="+a[2:]
			case f == nil && len(a) == 2:
				f, a = fs.Lookup(long), "--"+long
			}
		}
		out = append(out, a)
		// A value flag written without its value takes the next token.
		if f != nil && f.NoOptDefVal == "" && !strings.Contains(a, "=") &&
			(strings.HasPrefix(a, "--") || len(a) == 2) && i+1 < len(args) {
			i++
			out = append(out, args[i])
		}
	}
	return out
}

func longName(o slurmopts.Option) string {
	for _, n := range o.Names {
		if strings.HasPrefix(n, "--") {
			return n[2:]
		}
	}
	return ""
}

// unknownFlags returns the flag-shaped tokens in args (before the first
// positional) that the flag set does not define.
func unknownFlags(args []string, fs *pflag.FlagSet) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
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
// gpuAssignment publishes no per-rank device variable (REQ-GPU-003), so
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
func applyInteractiveFlags(opt *options, timeVal, gres, gpus, gpusPerNode, account, qos string) error {
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
	if opt.partition != "" || opt.pty || opt.immediate != "" || opt.input != "" {
		opt.interactiveFlagsSet = true
	}
	return nil
}

// applyAllocationResources records the resources a new allocation asks for.
// Inside an allocation a step cannot change them, and they are warned with
// the other allocation flags. mem is the last of --mem, --mem-per-cpu and
// --mem-per-gpu given.
func applyAllocationResources(opt *options, licenses, constraint, exclusive, exclude, tmp string, mem memChoice) error {
	opt.licenses, opt.constraint, opt.exclusive = licenses, constraint, exclusive
	hosts, err := submit.ExpandHosts("--exclude", exclude)
	if err != nil {
		return fmt.Errorf("srun: error: %w", err)
	}
	// -x also keeps a step inside an allocation off those hosts.
	opt.exclude, opt.req.Exclude = hosts, hosts
	for flag, v := range map[string]string{"--licenses": licenses, "--constraint": constraint, "--tmp": tmp} {
		if v != "" {
			opt.allocationOnly = append(opt.allocationOnly, flag)
		}
	}
	if mem.kind == "mem-per-cpu" || mem.kind == "mem-per-gpu" {
		opt.allocationOnly = append(opt.allocationOnly, "--"+mem.kind)
	}
	slices.Sort(opt.allocationOnly)
	if opt.tmp, err = submit.ConvertMem(tmp); err != nil {
		return fmt.Errorf("srun: error: --tmp: %w", err)
	}
	if mem.value != "" {
		if opt.mem, err = submit.ConvertMem(mem.value); err != nil {
			return fmt.Errorf("srun: error: --%s: %w", mem.kind, err)
		}
		if mem.kind != "mem" {
			opt.memKind = mem.kind
		}
	}
	if licenses != "" || constraint != "" || exclusive != "" || tmp != "" || mem.value != "" {
		opt.interactiveFlagsSet = true
	}
	return nil
}

// memChoice is the last memory flag given: kind is "mem", "mem-per-cpu" or
// "mem-per-gpu", value as written.
type memChoice struct{ kind, value string }

// memFlag is one of the three memory flags; each writes the shared choice
// as pflag parses it, so the choice ends as the last one given.
type memFlag struct {
	choice *memChoice
	kind   string
}

func (f memFlag) Set(v string) error {
	f.choice.kind, f.choice.value = f.kind, v
	return nil
}

func (f memFlag) String() string {
	if f.choice != nil && f.choice.kind == f.kind {
		return f.choice.value
	}
	return ""
}

func (f memFlag) Type() string { return "string" }
