package sbatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/fabricator"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

// maxPredictedNodes bounds the modelled allocation. sbatch runs on a shared login
// node, and the node count is driven straight from --nodes; without a ceiling a
// mistyped flag allocates gigabytes (or panics in make) to describe a job that
// qmaster would reject outright. A prediction past this many nodes conveys nothing
// the first few do not.
const maxPredictedNodes = 4096

// runtimeResolved names the Table A variables a dry run cannot know, because they
// come from the grant qmaster has not made yet. Their predicted values would be
// placeholders, so the placeholder is stated outright instead.
var runtimeResolved = map[string]string{
	"SLURM_JOB_ID":        "<assigned by qsub>",
	"SLURM_JOBID":         "<assigned by qsub>",
	"SLURM_ARRAY_JOB_ID":  "<assigned by qsub>",
	"SLURM_ARRAY_TASK_ID": "<this element's index>",
	"SLURM_JOB_NODELIST":  "<hosts from the grant>",
	"SLURM_NODELIST":      "<hosts from the grant>",
	"SLURM_JOB_GPUS":      "<device ids from the RSMAP grant>",
	"MASTER_ADDR":         "<master host from the grant>",
	"MASTER_PORT":         "<derived from the job id>",
}

// omittedAtRuntime are variables the real job exports that a prediction cannot
// produce at all, because they need the grant (a resolved master IP) or a live
// qstat query. Listing them explicitly keeps "the predictor cannot supply this"
// distinguishable from "the job will not have this".
var omittedAtRuntime = []struct{ key, note string }{
	{"SLURM_LAUNCH_NODE_IPADDR", "<master address, resolved from the grant>"},
}

// dryRun reports the submission instead of performing it: the exact qsub command
// line, how the request was resolved against the config, and the SLURM environment
// the job would see. It returns the process exit code -- dryrun.ExitFatal when the
// report itself proves the request cannot run, so the mode works as a gate.
//
// The report goes to stderr (REQ-LOG-003); only the KEY=VALUE environment block
// goes to stdout, so `sbatch 2>/dev/null` yields a diffable environment and a
// caller parsing stdout for a job id correctly finds none.
//
// Nothing here mutates cluster or filesystem state -- in particular it does not
// call materializeScript, whose wrapper mode would leave a spool directory next to
// the user's script for a job that is never submitted.
func dryRun(runner gedata.Runner, cfg *config.Config, self string, opt options, part config.Partition, slots int, par allocationRule, qargs []string, stdout, stderr io.Writer) int {
	w := &section{out: stderr}
	fmt.Fprintln(stderr, dryrun.Banner("sbatch"))

	script, scriptNote := scriptDisplay(cfg, self, opt)
	w.head("would submit")
	fmt.Fprintf(stderr, "  %s\n", renderQsub(qargs, script, opt.scriptArgs))

	pe := peFacts(runner, cfg, part.PE)

	ntasks, cpusPerTask := opt.resolveGeometry()
	w.head("request")
	w.kv("partition", fmt.Sprintf("%s -> queue %s, pe %s", opt.partition, part.Queue, part.PE))
	w.kv("slots", slotsExplain(part, slots, ntasks, cpusPerTask))
	w.kv("requested geometry", fmt.Sprintf("ntasks %d, cpus-per-task %d, nodes %s",
		ntasks, cpusPerTask, nodesText(opt.nodes)))
	if par.emit() {
		w.kv("allocation rule", fmt.Sprintf("-par %s (overrides PE %s's %s); -w e rejects the "+
			"job at submit if the layout is not schedulable", par.Value, part.PE, peRuleText(pe)))
	}
	if note := memoryNote(cfg, opt, par, slots); note != "" {
		w.kv("memory", note)
	}
	if n, ok := gpuRequest(opt); ok {
		w.kv("gpus per node", strconv.Itoa(n))
	}
	w.kv("script", script+scriptNote)

	nodes, spread, fatal := predictNodes(opt, pe, slots, par)
	w.kv("predicted spread", spread)
	if fatal != "" {
		w.kv("ERROR", fatal)
		fmt.Fprintf(stderr, "\nThis request cannot be dispatched as written; nothing was submitted.\n")
		return dryrun.ExitFatal
	}

	switch {
	case pe.err != nil:
		w.kv("pe config", "unavailable ("+pe.err.Error()+"); spread and control_slaves not checked")
	case !pe.knowsControlSlaves:
		w.kv("pe config", "qconf reported no control_slaves setting; multi-node srun support unverified")
	case !pe.controlSlaves && len(nodes) > 1:
		w.kv("pe config", "WARNING: control_slaves is not TRUE -- multi-node srun will fail in this PE")
	}

	res, err := predictEnv(runner, cfg, opt, part, nodes)
	if err != nil {
		fmt.Fprintf(stderr, "\njob environment: %v\n", err)
		if errors.Is(err, plan.ErrNoGPUs) {
			// Not a limit of the prediction: fabrication fails the same way on the
			// exec host, so this job would die at startup (REQ-ENC-005).
			fmt.Fprintf(stderr, "  This is not a gap in the dry run -- the job would fail at startup for the\n"+
				"  same reason. Request GPUs (--gpus-per-node / --gpus-per-task) or submit to a\n"+
				"  partition whose PE does not use task_policy gpu.\n")
		}
		return dryrun.ExitFatal
	}
	for _, warn := range res.Warnings {
		w.kv("warning", dryrun.Escape(warn))
	}

	if res.Disabled {
		w.head("job environment")
		fmt.Fprintf(stderr, "  SLURM_SHIM_DISABLE is set: this job receives NO SLURM_* variables\n"+
			"  (scrub-only mode). The dry run reports what the job gets, which is nothing.\n")
		return 0
	}

	// The environment block is the machine-readable payload and is the only thing
	// on stdout, so it can be diffed or sourced-for-inspection in isolation.
	w.head("job environment (fabricated on the master host when the job starts)")
	for _, kv := range res.Exports {
		value := kv.Value
		if p, ok := runtimeResolved[kv.Key]; ok {
			value = p
		}
		fmt.Fprintf(stdout, "%s=%s\n", kv.Key, dryrun.Escape(value))
	}
	for _, o := range omittedAtRuntime {
		fmt.Fprintf(stdout, "%s=%s\n", o.key, o.note)
	}
	if cfg.MemoryComplex != "" && opt.mem != "" {
		fmt.Fprintf(stdout, "SLURM_MEM_PER_NODE=<%s grant x node slots, read from qstat at run time>\n",
			cfg.MemoryComplex)
	}
	fmt.Fprintf(stderr, "  (written to stdout; everything else in this report is on stderr)\n")
	fmt.Fprintf(stderr, "\n  Values in <angle brackets> come from the real allocation. Everything else is\n"+
		"  exact for the predicted spread above; a different spread changes the task\n"+
		"  geometry.%s\n", spreadCaveat(par))
	return 0
}

// renderQsub renders the qsub command line with secret values masked. A submission
// carries `-v KEY=VALUE` pairs from --export, which is the documented SLURM idiom
// for injecting a token into a job; echoing the values would put them in terminal
// scrollback and in retained CI logs, which the real submit path never does.
func renderQsub(qargs []string, script string, scriptArgs []string) string {
	rendered := make([]string, 0, len(qargs)+1+len(scriptArgs))
	for i := 0; i < len(qargs); i++ {
		if qargs[i] == "-v" && i+1 < len(qargs) {
			rendered = append(rendered, "-v", dryrun.RedactAssignment(qargs[i+1]))
			i++
			continue
		}
		rendered = append(rendered, dryrun.Quote(qargs[i]))
	}
	rendered = append(rendered, dryrun.Quote(script))
	for _, a := range scriptArgs {
		rendered = append(rendered, dryrun.Quote(a))
	}
	// Already quoted per token, so join rather than re-quoting through Command.
	return "qsub " + strings.Join(rendered, " ")
}

// predictEnv fabricates the job environment for the predicted allocation, reusing
// the real fabricator so the listing cannot drift from what the PE hook exports.
//
// The environment lookup falls back to the process environment: qsub -V forwards
// the submit environment into the job, so the SLURM_SHIM_* overrides the fabricator
// honors (task policy, disable) are live inputs to what the job will actually see.
// A closed map would mispredict exactly the cases a user reaches for the overrides.
func predictEnv(runner gedata.Runner, cfg *config.Config, opt options, part config.Partition, nodes []fabricator.PredictedNode) (*fabricator.Result, error) {
	// Exact at submit time: qsub is told the queue, and GE records the invocation
	// directory and host as SGE_O_WORKDIR/SGE_O_HOST. Feeding them in means
	// SLURM_JOB_PARTITION resolves through the site's partition_aliases exactly as
	// it will at run time.
	fixed := map[string]string{
		"JOB_NAME":      opt.jobName,
		"PE":            part.PE,
		"QUEUE":         part.Queue,
		"SGE_O_WORKDIR": workdir(),
		"SGE_O_HOST":    hostname(runner),
	}
	// The grant does not exist yet, so these must not leak in from a parent job's
	// environment when sbatch is called from inside an allocation.
	for _, k := range []string{"JOB_ID", "PE_HOSTFILE", "NHOSTS", "NSLOTS", "SGE_TASK_ID", "RESTARTED"} {
		fixed[k] = ""
	}
	if opt.haveArray {
		// A predicted array reports the first element, whose coordinates are the ones
		// a user checks. GE task ids are 1-based and dense, and the range metadata is
		// what buildArrayArgs will put on the qsub line, so slurmArrayCoords resolves
		// exactly as it will in the job.
		fixed["SGE_TASK_ID"] = "1"
		fixed["SGE_TASK_FIRST"] = "1"
		fixed["SGE_TASK_LAST"] = strconv.Itoa((opt.arrayMax-opt.arrayMin)/opt.arrayStep + 1)
		fixed["SGE_TASK_STEPSIZE"] = "1"
		fixed["SLURM_ARRAY_BASE"] = strconv.Itoa(opt.arrayMin)
		fixed["SLURM_ARRAY_STEP"] = strconv.Itoa(opt.arrayStep)
	}

	return fabricator.Predict(fabricator.Options{
		Env: func(k string) string {
			if v, ok := fixed[k]; ok {
				return v
			}
			return os.Getenv(k)
		},
		Config:   cfg,
		Identity: gedata.RealIdentity{Runner: runner},
		NowUnix:  time.Now().Unix(),
	}, nodes)
}

// peConfig is what a dry run could learn about the target PE from qconf. err is
// non-nil when the lookup failed; knowsControlSlaves distinguishes "the PE says
// FALSE" from "qconf never told us", which must not be reported as a verdict.
type peConfig struct {
	allocationRule     string
	controlSlaves      bool
	knowsControlSlaves bool
	err                error
}

// peFacts reads the PE's config through the shared launch.PEConfig reader. qconf
// is read-only, so a dry run may call it; a failure is reported, never fatal,
// since the point of the mode is to answer without a working submission path.
func peFacts(runner gedata.Runner, cfg *config.Config, pe string) peConfig {
	if runner == nil || pe == "" {
		return peConfig{err: errors.New("no pe configured")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.QstatTimeout.Duration)
	defer cancel()
	parsed, err := launch.PEConfig(ctx, runner, pe)
	if err != nil {
		return peConfig{err: err}
	}
	v, ok := parsed["control_slaves"]
	return peConfig{
		allocationRule:     parsed["allocation_rule"],
		controlSlaves:      strings.EqualFold(v, "TRUE"),
		knowsControlSlaves: ok,
	}
}

// predictNodes models the allocation the environment is predicted for, and returns
// the spread, an explanation, and a non-empty fatal reason when the request cannot
// dispatch at all.
//
// It deliberately models only what is decidable at submit time. Every spread it
// returns is uniform: Grid Engine grants whole nodes under a fixed allocation_rule,
// so inventing a remainder node would fabricate a heterogeneous allocation that GE
// cannot produce -- and the fabricator would then emit a real warning about the
// non-uniformity the model invented.
func predictNodes(opt options, pe peConfig, slots int, par allocationRule) ([]fabricator.PredictedNode, string, string) {
	rule := strings.TrimSpace(pe.allocationRule)
	count, perNode, why := 0, 0, ""

	switch {
	// An emitted -par makes the spread a fact rather than a model: Grid Engine
	// grants exactly this many slots on exactly this many hosts, or refuses the
	// job at submit. Nothing below can improve on that, so it wins outright.
	case par.emit() && par.Value == "$pe_slots":
		count, perNode = 1, slots
		why = "qsub -par $pe_slots pins the job to one node"
	case par.emit():
		count, perNode = par.Nodes, slots/par.Nodes
		why = fmt.Sprintf("qsub -par %s pins %d slot(s) on each of %d node(s)",
			par.Value, perNode, count)
	case rule == "$pe_slots":
		count, perNode = 1, slots
		why = "allocation_rule $pe_slots pins the job to one node"
	case rule == "$round_robin":
		// GE hands out one slot per host in turn and wraps, so the spread is as wide
		// as the FREE HOSTS allow -- not as wide as the slot count. With --nodes the
		// request is already shaped for a host count and GE honors it (verified on
		// the test cluster: -N 3 --ntasks-per-node=2 lands 2 slots on each of 3
		// hosts). Without it the host count is not knowable at submit time, so the
		// one-slot-per-host reading is reported as an upper bound rather than a fact
		// -- the case docs/recipes/hydra documents, where a sweep entry silently ran
		// twice because slots scattered.
		if opt.nodes > 0 && opt.nodes <= slots {
			count, perNode = opt.nodes, slots/opt.nodes
			why = fmt.Sprintf("allocation_rule $round_robin over the requested %d node(s)", opt.nodes)
		} else {
			count, perNode = slots, 1
			why = "allocation_rule $round_robin spreads one slot per host; with no --nodes " +
				"this is the widest possible spread, and GE uses fewer hosts if fewer are free"
		}
	case rule != "" && rule != "$fill_up":
		n, err := strconv.Atoi(rule)
		switch {
		case err != nil || n < 1:
			// Unknown rule: fall through to the request-based assumption below.
		case slots%n != 0:
			return nil, fmt.Sprintf("allocation_rule %s grants %d slots per host", rule, n),
				fmt.Sprintf("slot count %d is not a multiple of allocation_rule %d; Grid Engine will not dispatch this request", slots, n)
		default:
			count, perNode = slots/n, n
			why = fmt.Sprintf("allocation_rule %s grants %d slot(s) on each node", rule, n)
		}
	}

	if count == 0 {
		count = opt.nodes
		if count < 1 || count > slots {
			count = 1
		}
		perNode = slots / count
		switch {
		case rule != "":
			why = fmt.Sprintf("allocation_rule %s is decided at dispatch; assuming %d node(s)", rule, count)
		case opt.nodes > 0:
			why = fmt.Sprintf("assuming %d node(s) from --nodes (the PE's allocation rule decides for real)", count)
		default:
			why = "assuming one node (no --nodes given; the PE's allocation rule decides for real)"
		}
	}

	capped := ""
	if count > maxPredictedNodes {
		count = maxPredictedNodes
		perNode = slots / count
		if perNode < 1 {
			perNode = 1
		}
		capped = fmt.Sprintf(" [capped at %d nodes for reporting]", maxPredictedNodes)
	}

	gpusPerNode := gpusPerPredictedNode(opt, count)
	nodes := make([]fabricator.PredictedNode, count)
	for i := range nodes {
		nodes[i] = fabricator.PredictedNode{
			Name:  fmt.Sprintf("node%02d", i+1),
			Slots: perNode,
			GPUs:  gpusPerNode,
		}
	}
	return nodes, fmt.Sprintf("%d node(s) x %d slots -- %s%s", count, perNode, why, capped), ""
}

// gpusPerPredictedNode sizes the per-node device count for the spread actually
// modelled. gpuRequest scales a --gpus-per-task request by the tasks --nodes would
// put on a host, which disagrees with a node count the allocation_rule decided.
func gpusPerPredictedNode(opt options, nodeCount int) int {
	if opt.haveGPUs {
		return opt.gpus
	}
	if !opt.haveGPUsPerTask {
		return 0
	}
	// Same helper the emitted -l <gres> request uses, so the report cannot claim a
	// per-node device count the submission did not ask for.
	return opt.gpusPerTask * opt.tasksOnNode(nodeCount)
}

// slotsExplain states where the slot count came from, since a partition's literal
// slots rule silently overrides the requested geometry.
// A literal rule never yields an allocation rule (parSpec declines to derive a
// width from a slot count the geometry did not produce), so the geometry really
// does not change anything on such a partition -- count or spread.
func slotsExplain(part config.Partition, slots, ntasks, cpusPerTask int) string {
	rule := strings.TrimSpace(part.Slots)
	if rule == "" || rule == "per-task" {
		return fmt.Sprintf("%d (rule \"per-task\": ntasks %d x cpus-per-task %d)", slots, ntasks, cpusPerTask)
	}
	return fmt.Sprintf("%d (partition slots rule %q -- the requested geometry does not change it)", slots, rule)
}

// scriptDisplay is the script path qsub would receive, plus a note on how the
// fabricator is injected. It asks materializeScript's own planner for the shape so
// the two cannot disagree about where a wrapper-mode spool lands.
func scriptDisplay(cfg *config.Config, self string, opt options) (string, string) {
	plan := submitPlan(cfg, opt)
	switch plan.mode {
	case submitPlain:
		return opt.script, " (submitted as-is; the PE start_proc_args hook fabricates)"
	case submitWrap:
		return plan.spoolPattern,
			" (generated from --wrap: " + dryrun.Escape(opt.wrap) + "; the PE hook fabricates)"
	default:
		return plan.spoolPattern,
			" (wrapper mode: runs `" + baseName(self) + " slurm-shim-env --export` then execs the stored original)"
	}
}

func nodesText(n int) string {
	if n < 1 {
		return "unset"
	}
	return strconv.Itoa(n)
}

// workdir is the directory sbatch was invoked from, which GE records as
// SGE_O_WORKDIR and the fabricator reports as SLURM_SUBMIT_DIR.
func workdir() string {
	d, err := os.Getwd()
	if err != nil {
		return ""
	}
	return d
}

// hostname resolves the submit host through the injected Identity seam, so a dry
// run under a fake runner does not reach the machine.
func hostname(runner gedata.Runner) string {
	h, err := gedata.RealIdentity{Runner: runner}.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// section renders the aligned key/value blocks of the report.
type section struct{ out io.Writer }

func (s *section) head(title string) { fmt.Fprintf(s.out, "\n%s:\n", title) }

func (s *section) kv(key, value string) { fmt.Fprintf(s.out, "  %-19s %s\n", key, value) }

// peRuleText names the PE's own allocation_rule for the report, so the user can
// see what the emitted -par is overriding. Falls back to a hedge when qconf could
// not be read -- claiming a rule we did not observe is the trap todo 005 closed.
func peRuleText(pe peConfig) string {
	if r := strings.TrimSpace(pe.allocationRule); r != "" {
		return r
	}
	return "configured allocation_rule (not read)"
}

// spreadCaveat is the closing note about how much of the report is a prediction.
// With a rule emitted the spread is pinned at submit, so the old blanket warning
// ("Grid Engine decides the spread at dispatch, and --nodes is not translated to
// qsub") would now be false.
func spreadCaveat(par allocationRule) string {
	if par.emit() {
		return " The spread above is pinned by qsub -par, not modelled:\n" +
			"  Grid Engine grants it or refuses the job at submit."
	}
	return " Note that Grid Engine decides the spread at dispatch, and no\n" +
		"  allocation rule was emitted for this request -- the PE's own rule places the\n" +
		"  nodes, and --nodes only feeds the slot count."
}

// memoryNote spells out the per-node memory a pinned spread actually yields.
//
// It exists because pinning the layout silently changes that number: --mem is per
// node on SLURM, but the memory complex is per slot on most Grid Engine sites, so
// a job whose 6 slots used to land on one host (6 x the grant) gets 2 x on each of
// three under -par 2. The multiplication is only right for a per-slot consumable,
// which is a site setting the shim does not read, so the note says so rather than
// asserting a number it cannot verify.
func memoryNote(cfg *config.Config, opt options, par allocationRule, slots int) string {
	if !par.emit() || opt.mem == "" || cfg.MemoryComplex == "" {
		return ""
	}
	perNode := slots / par.Nodes
	return fmt.Sprintf("%s=%s x %d slot(s)/node (the per-node ceiling, if %s is a "+
		"per-slot consumable at your site; pinning the spread changes it)",
		cfg.MemoryComplex, opt.mem, perNode, cfg.MemoryComplex)
}
