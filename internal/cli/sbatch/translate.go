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

// buildQsubArgs assembles the qsub argv (without the trailing script + args) and
// any warnings the caller must surface on stderr (currently a batch-level -o/-e
// path GE cannot express). -terse yields just the job id on stdout
// (REQ-SBT-003). cfg supplies the site's GE complex names for the resource (-l)
// mapping.
func buildQsubArgs(cfg *config.Config, opt options, part config.Partition, slots int) ([]string, []string) {
	var warns []string
	args := []string{"-terse", "-q", part.Queue, "-pe", part.PE, strconv.Itoa(slots)}
	if opt.jobName != "" {
		args = append(args, "-N", opt.jobName)
	}
	if opt.output != "" {
		p, w := batchPath(opt, opt.output, "out")
		args = append(args, "-o", p)
		warns = appendWarn(warns, w)
	} else if !opt.haveArray {
		// SLURM default: stdout goes to slurm-%j.out in the working directory.
		// GE's default (<jobname>.o<id>) both names differently and, worse, is
		// opened relative to the job cwd even when unwritable. Arrays keep GE's
		// per-task default naming.
		args = append(args, "-o", "slurm-$JOB_ID.out")
	}
	if opt.errorPath != "" {
		p, w := batchPath(opt, opt.errorPath, "err")
		args = append(args, "-e", p)
		warns = appendWarn(warns, w)
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
	// Outside the guard on purpose: the case with NO ids emits no -hold_jid at
	// all, which is the failure most worth reporting.
	warns = appendWarn(warns, dependencyWarning(opt.depSpec, opt.holdJIDs))
	return args, warns
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
		return opt.gpusPerTask * opt.tasksPerNode(), true
	}
	return 0, false
}

// tasksPerNode is how many tasks land on one node, used to scale a per-task GPU
// request. --ntasks-per-node states it outright; otherwise it is derived from the
// resolved task count spread over the requested nodes (rounding up, since a
// remainder still needs devices on some node). Defaults to one task per node.
func (o options) tasksPerNode() int {
	if o.havePerNode && o.ntasksPerNode > 0 {
		return o.ntasksPerNode
	}
	ntasks, _ := o.resolveGeometry()
	nodes := o.nodes
	if nodes < 1 {
		nodes = 1
	}
	if perNode := (ntasks + nodes - 1) / nodes; perNode > 0 {
		return perNode
	}
	return 1
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

// batchPath renders a batch-level -o/-e path for qsub, substituting a
// GE-expressible one when the requested pattern cannot be represented.
//
// SLURM's %a is the job's own array index, but GE only offers $TASK_ID, which
// numbers the tasks of the range we actually submit (a dense 1..N). Those agree
// only for a 1-based, unit-step array; for anything else -- notably the 0-based
// arrays submitit and Hydra emit, and any stepped array -- $TASK_ID is off, so
// the rendered path would name another task's file, or a directory that does not
// exist (Hydra uses .../%A_%a/...), which GE fails with Eqw rather than creating.
//
// Rather than drop the argument (which sends output to GE's cwd-relative default
// under a name derived from a shim-internal script, and can itself Eqw when the
// submit dir is unwritable), keep the longest literal directory prefix the user
// asked for -- that directory has to exist for SLURM too -- and put a
// GE-expressible per-task file in it.
// appendWarn adds w to warns when it is non-empty and not already present, so a
// job whose -o and -e are both rewritten reports the substitution once.
func appendWarn(warns []string, w string) []string {
	if w == "" {
		return warns
	}
	for _, have := range warns {
		if have == w {
			return warns
		}
	}
	return append(warns, w)
}

// stream is "out" or "err" so the two descriptors can never land on one file.
// The second result is a warning for the caller to surface, empty when the
// requested path was used as-is.
func batchPath(opt options, path, stream string) (string, string) {
	if !opt.haveArray || (opt.arrayMin == 1 && opt.arrayStep == 1) || !referencesArrayTask(path) {
		return translateOutputPath(path), ""
	}
	dir := literalDir(path)
	return dir + "slurm-$JOB_ID.$TASK_ID." + stream,
		"Grid Engine cannot express %a for this array; batch-level output goes to " +
			dir + "slurm-$JOB_ID.$TASK_ID.{out,err} (per-task files written by srun are unaffected)"
}

// literalDir returns the leading directory part of a pattern that contains no
// %-verb, with a trailing separator (empty when the first component already
// varies). Everything below it may differ per task, so it is the deepest
// directory GE can be told to write into. A %% escape is literal, so it neither
// ends the prefix nor survives into it.
func literalDir(p string) string {
	var b strings.Builder
	cut := 0
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '/':
			b.WriteByte('/')
			cut = b.Len()
		case '%':
			if i+1 < len(p) && p[i+1] == '%' {
				b.WriteByte('%')
				i++
				continue
			}
			return b.String()[:cut]
		default:
			b.WriteByte(p[i])
		}
	}
	return b.String()[:cut]
}

// referencesArrayTask reports whether a SLURM output pattern uses %a, honoring
// the %% escape and SLURM's optional zero-pad width (%3a).
func referencesArrayTask(p string) bool {
	for i := 0; i < len(p); i++ {
		if p[i] != '%' || i+1 >= len(p) {
			continue
		}
		i++
		for i < len(p) && p[i] >= '0' && p[i] <= '9' {
			i++
		}
		if i < len(p) && p[i] == 'a' {
			return true
		}
	}
	return false
}

// translateOutputPath rewrites SLURM output-pattern verbs in an sbatch -o/-e path
// into the GE pseudo-variables qsub expands at run time (qsub -o/-e understand
// $JOB_ID/$TASK_ID/$JOB_NAME/$USER/$HOSTNAME, not SLURM's %-tokens; a verbatim
// "%j" would become a literal filename). %t/%n (task rank / node id) have no
// batch-level meaning and become 0. %a maps to GE's $TASK_ID, which is only
// equivalent for a 1-based unit-step array -- batchPath is the authority on when
// this may be called with an array path at all. An unknown verb keeps its % and
// letter; a zero-pad width (%3a) is dropped, since GE cannot pad.
func translateOutputPath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		if p[i] != '%' || i+1 >= len(p) {
			b.WriteByte(p[i])
			continue
		}
		i++
		// SLURM allows a zero-pad width (%3a, %5j). GE cannot pad, so the width is
		// dropped, but the verb must still expand -- writing it back verbatim would
		// give every task one literal filename.
		for i < len(p) && p[i] >= '0' && p[i] <= '9' {
			i++
		}
		if i >= len(p) {
			b.WriteByte('%')
			break
		}
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
