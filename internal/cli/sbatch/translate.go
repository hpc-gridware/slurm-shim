package sbatch

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// resolveGeometry derives the SLURM task geometry from the parsed options,
// applying SLURM's defaults: -N without -n/--ntasks-per-node is one task per
// node; --ntasks wins over an inconsistent --ntasks-per-node.
func (o options) resolveGeometry() (ntasks, cpusPerTask int) {
	cpusPerTask = o.cpusPerTask
	if cpusPerTask < 1 {
		cpusPerTask = 1
	}
	switch {
	case o.haveNtasks:
		ntasks = o.ntasks
	case o.nodes > 0 && o.havePerNode:
		ntasks = o.nodes * o.ntasksPerNode
	case o.nodes > 0:
		ntasks = o.nodes
	case o.havePerNode:
		ntasks = o.ntasksPerNode
	default:
		ntasks = 1
	}
	if ntasks < 1 {
		ntasks = 1
	}
	return ntasks, cpusPerTask
}

// computeSlots applies the partition's slots rule (REQ-SBT-002), the inverse of
// task policy N3. A literal integer pins the slot count; "per-task" (and the
// empty default) multiplies ntasks by cpus_per_task.
func computeSlots(opt options, part config.Partition) (int, error) {
	ntasks, cpt := opt.resolveGeometry()
	rule := strings.TrimSpace(part.Slots)
	switch rule {
	case "", "per-task":
		return ntasks * cpt, nil
	default:
		n, err := strconv.Atoi(rule)
		if err != nil {
			return 0, fmt.Errorf("sbatch: error: partition slots rule %q is neither an integer nor \"per-task\"", rule)
		}
		return n, nil
	}
}

// parseJobID extracts the base job id from qsub -terse output. A plain job
// prints "<id>"; an array job prints "<id>.<first>-<last>:<step>" (REQ-SBT-003).
func parseJobID(terse string) string {
	s := strings.TrimSpace(terse)
	if s == "" {
		return ""
	}
	s = strings.SplitN(s, "\n", 2)[0]
	return strings.SplitN(s, ".", 2)[0]
}

// buildQsubArgs assembles the qsub argv (without the trailing script + args).
// -terse yields just the job id on stdout (REQ-SBT-003). cfg supplies the site's
// GE complex names for the resource (-l) mapping.
func buildQsubArgs(cfg *config.Config, opt options, part config.Partition, slots int) []string {
	args := []string{"-terse", "-q", part.Queue, "-pe", part.PE, strconv.Itoa(slots)}
	if opt.jobName != "" {
		args = append(args, "-N", opt.jobName)
	}
	if opt.output != "" {
		args = append(args, "-o", translateOutputPath(opt.output))
	} else if !opt.haveArray {
		// SLURM default: stdout goes to slurm-%j.out in the working directory.
		// GE's default (<jobname>.o<id>) both names differently and, worse, is
		// opened relative to the job cwd even when unwritable. Arrays keep GE's
		// per-task default naming (SLURM's slurm-%A_%a.out has 0-based %a that a
		// batch-level GE path cannot express; submitit always passes --output).
		args = append(args, "-o", "slurm-$JOB_ID.out")
	}
	if opt.errorPath != "" {
		args = append(args, "-e", translateOutputPath(opt.errorPath))
	} else {
		// SLURM merges stderr into the stdout file unless --error is given; GE
		// instead defaults stderr to a separate <jobname>.e<id> in the job cwd,
		// which also fails the whole job (Eqw, "opening input/output file") when
		// that cwd is unwritable. -j y restores SLURM's join semantics.
		args = append(args, "-j", "y")
	}
	if opt.chdir != "" {
		args = append(args, "-wd", opt.chdir)
	} else {
		// SLURM runs the batch script in the directory sbatch was invoked from;
		// GE defaults to $HOME, so -cwd restores SLURM's default working dir
		// (qsub runs in the user's cwd, making -cwd exactly the submit dir).
		args = append(args, "-cwd")
	}
	args = append(args, exportArgs(opt.exportSpec)...)
	if opt.haveArray {
		args = append(args, buildArrayArgs(opt)...)
	}
	if opt.haveSignal {
		// A --signal request means the job wants a catchable warning before it is
		// killed/preempted (submitit's checkpoint-then-requeue). GE delivers that
		// via -notify (SIGUSR2 before a kill/reschedule -- exactly submitit's
		// default preempt signal), and the job must be rerunnable (-r y) so the
		// scheduler restarts it after the requeue. scancel --signal / scontrol
		// requeue then map to `qmod -rj`, which delivers SIGUSR2 and reschedules.
		args = append(args, "-notify", "-r", "y")
	}
	// Carry the task/device binding intent to the step: srun reads SLURM_GPU_BIND
	// (as it would on SLURM, where an #SBATCH --gpu-bind propagates that way), and
	// --gpus-per-task implies per-task binding.
	if b := gpuBindSpec(opt); b != "" {
		args = append(args, "-v", "SLURM_GPU_BIND="+b)
	}
	if opt.haveGPUsPerTask {
		args = append(args, "-v", "SLURM_GPUS_PER_TASK="+strconv.Itoa(opt.gpusPerTask))
	}
	if l := buildResourceList(cfg, opt); l != "" {
		args = append(args, "-l", l)
	}
	if len(opt.holdJIDs) > 0 {
		args = append(args, "-hold_jid", strings.Join(opt.holdJIDs, ","))
	}
	return args
}

// exportArgs maps SLURM's --export to qsub environment flags. SLURM's default
// (and explicit ALL) forwards the whole submit environment, which is qsub -V;
// verified live on OCS 9.1: -V tolerates exported bash functions (Lmod) and
// flattens newline values to spaces rather than erroring, so no env scrubbing
// is needed. NONE forwards nothing (GE's own default). An explicit list maps to
// one -v per assignment; GE adds those on top of its minimal env rather than
// SLURM's only-these semantics -- close enough, and ALL,VAR=v composes.
func exportArgs(spec string) []string {
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

// gpuBindSpec is the SLURM_GPU_BIND value to publish for the job: an explicit
// --gpu-bind wins, otherwise --gpus-per-task implies per_task binding (as it
// does on SLURM). Empty when neither was given, leaving the site default.
func gpuBindSpec(opt options) string {
	if b := strings.TrimSpace(opt.gpuBind); b != "" {
		return b
	}
	if opt.haveGPUsPerTask {
		return "per_task:" + strconv.Itoa(opt.gpusPerTask)
	}
	return ""
}

// gpuRequest resolves the per-node GPU count to request. --gpus/--gpus-per-node
// are already per-node; --gpus-per-task is per task, so it scales by the tasks
// placed on a node. Returns ok=false when no GPU request was made.
func gpuRequest(opt options) (int, bool) {
	if opt.haveGPUs {
		return opt.gpus, true
	}
	if opt.haveGPUsPerTask {
		perNode := opt.ntasksPerNode
		if !opt.havePerNode {
			// resolveGeometry's default is one task per node.
			perNode = 1
		}
		return opt.gpusPerTask * perNode, true
	}
	return 0, false
}

// buildResourceList assembles a single comma-joined GE -l value from the mapped
// resource requests. Wall time -> h_rt (with s_rt as a pre-kill grace when a
// --signal lead time is given, so the job gets a catchable SIGUSR1 before the
// hard kill); memory -> the site's memory complex; GPUs -> the site's gres
// complex. Empty when nothing was requested.
func buildResourceList(cfg *config.Config, opt options) string {
	var l []string
	if opt.haveTime {
		l = append(l, "h_rt="+strconv.Itoa(opt.timeSec))
		if opt.haveSignal && opt.signalDelay > 0 && opt.signalDelay < opt.timeSec {
			l = append(l, "s_rt="+strconv.Itoa(opt.timeSec-opt.signalDelay))
		}
	}
	if opt.mem != "" && cfg.MemoryComplex != "" {
		l = append(l, cfg.MemoryComplex+"="+opt.mem)
	}
	if n, ok := gpuRequest(opt); ok && cfg.GPU.GresComplex != "" {
		l = append(l, cfg.GPU.GresComplex+"="+strconv.Itoa(n))
	}
	return strings.Join(l, ",")
}

// buildArrayArgs translates a SLURM --array request into GE array flags. GE task
// ids are 1-based and contiguous, so an N-element SLURM array (any origin/step)
// becomes a dense `-t 1-N`; the SLURM origin/step travel to the job as
// SLURM_ARRAY_BASE/STEP env vars, and the fabricator maps each GE task back to
// its SLURM index (slurmArrayCoords). `%p` throttling maps to GE `-tc`.
func buildArrayArgs(opt options) []string {
	count := (opt.arrayMax-opt.arrayMin)/opt.arrayStep + 1
	args := []string{"-t", "1-" + strconv.Itoa(count)}
	if opt.arrayThrottle > 0 {
		args = append(args, "-tc", strconv.Itoa(opt.arrayThrottle))
	}
	// Separate -v per var: values are integers, so the comma separator that -v
	// uses between assignments is irrelevant, and this keeps quoting trivial.
	args = append(args, "-v", "SLURM_ARRAY_BASE="+strconv.Itoa(opt.arrayMin))
	args = append(args, "-v", "SLURM_ARRAY_STEP="+strconv.Itoa(opt.arrayStep))
	return args
}

// translateOutputPath rewrites SLURM output-pattern verbs in an sbatch -o/-e path
// into the GE pseudo-variables qsub expands at run time (qsub -o/-e understand
// $JOB_ID/$TASK_ID/$JOB_NAME/$USER/$HOSTNAME, not SLURM's %-tokens; a verbatim
// "%j" would become a literal filename). %t/%n (task rank / node id) have no
// batch-level meaning and become 0. For an array, %a maps to GE's 1-based
// $TASK_ID; the real per-task output is written by srun at the SLURM 0-based path,
// so any array batch-level file is a harmless secondary stream. An unknown %x is
// left verbatim.
func translateOutputPath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		if p[i] != '%' || i+1 >= len(p) {
			b.WriteByte(p[i])
			continue
		}
		i++
		switch p[i] {
		case 'j', 'A':
			b.WriteString("$JOB_ID")
		case 'a':
			b.WriteString("$TASK_ID")
		case 'x':
			b.WriteString("$JOB_NAME")
		case 'u':
			b.WriteString("$USER")
		case 'N':
			b.WriteString("$HOSTNAME")
		case 't', 'n':
			b.WriteByte('0')
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(p[i])
		}
	}
	return b.String()
}

// buildWrapper generates the wrapper-mode bash script (SI-57, REQ-SBT-004): it
// evaluates the fabricator's exported environment, then execs the stored
// original script verbatim so its shebang and argv survive untouched. Paths are
// single-quote-escaped so a name containing a quote cannot break out.
func buildWrapper(shimPath, storedScript string) string {
	return "#!/bin/bash\n" +
		"# Generated by slurm-shim sbatch (wrapper mode, SI-57). Do not edit.\n" +
		"eval \"$(" + shellQuote(shimPath) + " slurm-shim-env --export)\"\n" +
		"exec " + shellQuote(storedScript) + " \"$@\"\n"
}

// shellQuote wraps a value in single quotes, replacing each embedded quote with
// the POSIX-safe sequence quote-backslash-quote-quote.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
