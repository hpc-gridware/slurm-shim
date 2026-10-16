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

package sbatch

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/slurmopts"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

// options is the parsed sbatch request.
type options struct {
	nodes         int
	ntasks        int
	ntasksPerNode int
	cpusPerTask   int
	haveNtasks    bool
	havePerNode   bool

	partition string
	jobName   string
	account   string
	output    string
	errorPath string
	chdir     string
	wrap      string

	// Array request (--array). arrayMin/Max/Step are the SLURM (submitit is
	// 0-based) coordinates; arrayThrottle is the %<n> concurrency cap.
	haveArray     bool
	arrayMin      int
	arrayMax      int
	arrayStep     int
	arrayThrottle int

	// Resource / signal / dependency requests (Phase 4). Mapped to GE -l /
	// -hold_jid in buildQsubArgs, which resolves the site's complex names.
	haveTime bool
	timeSec  int
	mem      string // GE-formatted memory value ("4G"), "" if unset
	haveGPUs bool
	gpus     int
	// --gpus-per-task is a per-task request; the node-level GPU count it implies
	// is gpusPerTask * ntasks-per-node. It also binds each task to its own
	// devices (SLURM implies --gpu-bind=per_task), carried to the step via
	// SLURM_GPUS_PER_TASK / SLURM_GPU_BIND.
	haveGPUsPerTask bool
	gpusPerTask     int
	gpuBind         string
	haveSignal      bool
	signalDelay     int      // seconds before the time limit to deliver the signal
	holdJIDs        []string // predecessor ids for -hold_jid
	depSpec         string   // raw --dependency value, for dependencyWarning
	exportSpec      string   // --export value; "" means the SLURM default ALL

	// testOnly is SLURM's --test-only: report what would be submitted and submit
	// nothing. It is OR'd with SLURM_SHIM_DRY_RUN so callers that can only inject
	// #SBATCH directives or argv (Hydra, submitit, CI templates) can reach the
	// mode without controlling the environment.
	testOnly bool

	// parsable prints the job id ALONE, which is how scripts capture it:
	//   jid=$(sbatch --parsable job.sh)
	// Without it the caller gets "Submitted batch job 123" and has to parse
	// prose -- and when the flag was merely warn-and-ignored, the job still ran,
	// so the caller silently held a string that was not an id.
	parsable bool
	// wait is -W/--wait: block until the job ends and exit with its exit
	// status, the highest over an array's tasks.
	wait bool

	// Cluster resources beyond geometry (-L, -C, --exclusive, -w, -x, --tmp),
	// checked against the cluster's complexes and resolved by submit.
	licenses      string
	constraint    string
	exclusive     bool
	exclusiveMode string // --exclusive=user|mcs|topo, which has no GE form
	nodelist      string // -w hostlist, as given
	exclude       string // -x hostlist, as given
	tmp           string // GE-formatted --tmp
	// Filled by run() once the cluster was consulted, for buildQsubArgs: the
	// -l entries for the above, whether -w e can judge them, and the qsub
	// job-control flags.
	resourceEntries []string
	unverifiable    string // why -w e must not judge the job, "" when it may
	controlArgs     []string
	// memKind is which memory flag set mem: "mem" (per node), "mem-per-cpu" or
	// "mem-per-gpu" (per unit, scaled by submit.MemoryForScope at submit).
	memKind string

	// Job control (Phase 1c): -H, --begin, --requeue/--no-requeue, mail and
	// --nice map to qsub -h, -a, -r, -m/-M and -p.
	hold     bool
	begin    string
	requeue  string // "" unset, "y" or "n"
	mailType string
	mailUser string
	haveNice bool
	nice     int

	// Hot spares (shim extensions, also accepted on #SHIM lines): --x-spares=k
	// adds k idle nodes that replace a lost node during a step; --x-elastic
	// picks which steps do that (auto, on, off).
	spares     int
	haveSpares bool
	elastic    string

	script     string   // script file path (first non-flag token)
	scriptArgs []string // tokens after the script
}

// boolLong are the long flags that take NO value. They are also listed in
// knownLong (which gates recognition), but parseFlags checks this set first so a
// boolean flag does not consume the following token -- which would be the script
// path.
var boolLong = map[string]bool{
	"test-only": true,
	"parsable":  true,
	"wait":      true,
	"hold":      true,
	"requeue":   true, "no-requeue": true,
}

// optionalLong are the long flags whose value may only be attached
// (--exclusive, --exclusive=user): bare, they take none, so the next token --
// often the script -- is never consumed.
var optionalLong = map[string]bool{
	"exclusive": true,
	"nice":      true,
}

// setBool applies a valueless long flag.
func setBool(o *options, name string) error {
	switch name {
	case "test-only":
		o.testOnly = true
		return nil
	case "parsable":
		o.parsable = true
		return nil
	case "wait":
		o.wait = true
		return nil
	case "hold":
		o.hold = true
		return nil
	case "requeue":
		o.requeue = "y"
		return nil
	case "no-requeue":
		o.requeue = "n"
		return nil
	}
	return fmt.Errorf("sbatch: error: --%s is not a boolean flag", name)
}

// truthyFlag reads the value of a --flag=VALUE form given for a boolean flag, so
// `--test-only=false` means what it says rather than enabling the mode.
func truthyFlag(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

// knownLong is the set of long flags the translator handles. All of them take a
// value -- parseFlags consumes the next token when the "=" form was not used --
// EXCEPT those also listed in boolLong, which take none. Anything absent is
// warn-and-ignored (REQ-SBT-001).
var knownLong = map[string]bool{
	"test-only": true, "parsable": true, "wait": true,
	"hold": true, "requeue": true, "no-requeue": true,
	"exclusive": true, "nice": true,
	"licenses": true, "constraint": true, "exclude": true, "nodelist": true,
	"mem-per-gpu": true, "tmp": true,
	"begin": true, "mail-type": true, "mail-user": true,
	"nodes": true, "ntasks": true, "ntasks-per-node": true, "cpus-per-task": true,
	"partition": true, "job-name": true, "output": true, "error": true,
	"chdir": true, "wrap": true, "array": true,
	"time": true, "mem": true, "mem-per-cpu": true,
	"gpus": true, "gpus-per-node": true, "gres": true,
	"gpus-per-task": true, "gpu-bind": true,
	"signal": true, "dependency": true, "export": true,
	"account":  true,
	"x-spares": true, "x-elastic": true,
	// Accepted and intentionally ignored (GE has no distinct behavior to map):
	"open-mode": true, "wckey": true,
}

// sbatchOpts is SLURM's sbatch option table: the arity of every option, used
// to step over the ones the shim does not translate.
var sbatchOpts = slurmopts.For("sbatch")

var shortToLong = map[byte]string{
	'N': "nodes", 'n': "ntasks", 'c': "cpus-per-task", 'p': "partition",
	'J': "job-name", 'o': "output", 'e': "error", 'D': "chdir",
	'a': "array", 't': "time", 'd': "dependency", 'A': "account",
	'L': "licenses", 'C': "constraint", 'x': "exclude", 'w': "nodelist", 'b': "begin",
}

// containerValueFlags are the Pyxis --container-* directives known to take a
// value, so a space-form value is consumed rather than mistaken for the script.
// Boolean container flags (e.g. --container-remap-root, --container-readonly)
// are deliberately absent so they consume nothing. All are warn-and-ignored;
// this table only fixes their arity (REQ-SBT-001).
var containerValueFlags = map[string]bool{
	"container-image": true, "container-mounts": true, "container-workdir": true,
	"container-name": true, "container-save": true, "container-env": true,
	"container-entrypoint": true, "container-mount-home": true,
}

// parseFlags folds command-line tokens into options, returning warnings for
// unknown flags. The first non-flag token is the job script; the rest are its
// arguments.
func parseFlags(tokens []string) (options, []string, error) {
	return parseArgs(tokens, 0)
}

// parseArgs is parseFlags for #SBATCH directive tokens followed by command-line
// tokens: the first nDirective tokens come from directives, the rest from the
// command line, which therefore wins on conflicts.
//
// A directive block never holds the job script, so two rules apply inside it
// that cannot apply on the command line:
//
//   - A bare word after an unsupported option is that option's value
//     (`#SBATCH --qos normal`), and is consumed with it.
//   - Any other bare word is an error. Treating it as the script, as the
//     command-line rules would, made every later directive and every
//     command-line flag a script argument -- a job submitted without its GPU
//     request, with nothing but an "unknown directive" warning to show for it.
//
// A directive's value is never taken from the command-line tokens.
func parseArgs(tokens []string, nDirective int) (options, []string, error) {
	var opt options
	var warns []string
	warned := map[string]bool{}

	i := 0
	next := func() (string, bool) {
		if i+1 < len(tokens) {
			i++
			return tokens[i], true
		}
		return "", false
	}
	// skipDirectiveValue consumes the bare word after an unsupported option when
	// both come from the directives.
	skipDirectiveValue := func() {
		if i+1 < nDirective && !strings.HasPrefix(tokens[i+1], "-") {
			i++
		}
	}
	// skipSlurmValue steps over the value of an option the shim does not
	// translate, when SLURM's own table says the option takes one in this
	// spelling. It reports whether the table knew the option. A directive's
	// value is never taken from the command line.
	skipSlurmValue := func(tok string) bool {
		if _, known := sbatchOpts.Lookup(tok); !known {
			return false
		}
		if sbatchOpts.TakesNext(tok) && (i >= nDirective || i+1 < nDirective) {
			_, _ = next()
		}
		return true
	}

	for ; i < len(tokens); i++ {
		tok := tokens[i]
		switch {
		case i < nDirective && (tok == "-" || !strings.HasPrefix(tok, "-")):
			if strings.EqualFold(tok, "hetjob") || strings.EqualFold(tok, "packjob") {
				return opt, warns, fmt.Errorf("sbatch: error: heterogeneous jobs (#SBATCH %s) are not supported", tok)
			}
			return opt, warns, fmt.Errorf("sbatch: error: #SBATCH directives contain the stray word %q; "+
				"write each option as --name=value", tok)
		case opt.script != "":
			opt.scriptArgs = append(opt.scriptArgs, tok)
		case strings.HasPrefix(tok, "--"):
			name := strings.TrimPrefix(tok, "--")
			val := ""
			hasVal := false
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name, val, hasVal = name[:eq], name[eq+1:], true
			}
			if !knownLong[name] {
				if !warned[name] {
					warns = append(warns, "unknown directive --"+name+" ignored")
					warned[name] = true
				}
				// A SLURM option steps over exactly its value, per SLURM's own
				// table (REQ-RUN-005), so `sbatch --qos normal job.sh` does not
				// submit "normal". Known value-taking Pyxis flags consume theirs;
				// any other unknown flag consumes a bare word only inside a
				// directive block.
				switch {
				case hasVal:
				case skipSlurmValue(tok):
				case containerValueFlags[name]:
					_, _ = next()
				case i < nDirective:
					skipDirectiveValue()
				}
				continue
			}
			// Boolean flags take no value: consuming the next token would swallow
			// the script path.
			if boolLong[name] {
				if hasVal && !truthyFlag(val) {
					continue
				}
				if err := setBool(&opt, name); err != nil {
					return opt, warns, err
				}
				continue
			}
			if optionalLong[name] {
				if err := setLong(&opt, name, val); err != nil {
					return opt, warns, err
				}
				continue
			}
			if !hasVal {
				v, ok := next()
				if !ok {
					return opt, warns, fmt.Errorf("sbatch: error: --%s requires a value", name)
				}
				val = v
			}
			if err := setLong(&opt, name, val); err != nil {
				return opt, warns, err
			}
			if name == "gres" {
				if other := nonGPUGres(val); len(other) > 0 {
					warns = append(warns, "--gres "+strings.Join(other, ",")+" ignored: only gpu gres is "+
						"translated (licenses: --licenses; other resources: a site complex)")
				}
			}
		case tok == "-W":
			opt.wait = true // -W is --wait; -H is --hold: the short flags without a value
		case tok == "-H":
			opt.hold = true
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			flag := tok[1]
			long, ok := shortToLong[flag]
			if !ok {
				if !warned[string(flag)] {
					warns = append(warns, "unknown directive -"+string(flag)+" ignored")
					warned[string(flag)] = true
				}
				if !skipSlurmValue(tok) && len(tok) == 2 && i < nDirective {
					skipDirectiveValue()
				}
				continue
			}
			val := tok[2:] // -N4 form
			if val == "" {
				v, ok := next()
				if !ok {
					return opt, warns, fmt.Errorf("sbatch: error: -%c requires a value", flag)
				}
				val = v
			}
			if err := setLong(&opt, long, val); err != nil {
				return opt, warns, err
			}
		default:
			opt.script = tok
		}
	}
	return opt, warns, nil
}

func setLong(opt *options, name, val string) error {
	atoi := func() (int, error) {
		n, err := strconv.Atoi(val)
		if err != nil {
			return 0, fmt.Errorf("sbatch: error: --%s: invalid integer %q", name, val)
		}
		return n, nil
	}
	switch name {
	case "nodes":
		n, err := atoi()
		if err != nil {
			return err
		}
		opt.nodes = n
	case "ntasks":
		n, err := atoi()
		if err != nil {
			return err
		}
		opt.ntasks, opt.haveNtasks = n, true
	case "ntasks-per-node":
		n, err := atoi()
		if err != nil {
			return err
		}
		opt.ntasksPerNode, opt.havePerNode = n, true
	case "cpus-per-task":
		n, err := atoi()
		if err != nil {
			return err
		}
		opt.cpusPerTask = n
	case "x-spares":
		n, err := atoi()
		if err != nil {
			return err
		}
		if n < 0 {
			return fmt.Errorf("sbatch: error: --x-spares must not be negative (got %d)", n)
		}
		opt.spares, opt.haveSpares = n, true
	case "x-elastic":
		switch v := strings.ToLower(val); v {
		case "on", "off", "auto":
			opt.elastic = v
		default:
			return fmt.Errorf("sbatch: error: --x-elastic=%q: expected on, off or auto", val)
		}
	case "partition":
		opt.partition = val
	case "job-name":
		opt.jobName = val
	case "account":
		opt.account = val
	case "output":
		opt.output = val
	case "error":
		opt.errorPath = val
	case "chdir":
		opt.chdir = val
	case "wrap":
		opt.wrap = val
	case "array":
		min, max, step, throttle, err := parseArraySpec(val)
		if err != nil {
			return err
		}
		opt.haveArray = true
		opt.arrayMin, opt.arrayMax, opt.arrayStep, opt.arrayThrottle = min, max, step, throttle
	case "time":
		sec, err := submit.ParseSlurmTime(val)
		if err != nil {
			return fmt.Errorf("sbatch: error: %w", err)
		}
		opt.timeSec, opt.haveTime = sec, true
	case "mem", "mem-per-cpu", "mem-per-gpu":
		// Last wins, as for every flag here; the per-unit kinds are scaled to
		// the memory complex's scope once the job's shape is known.
		mem, err := submit.ConvertMem(val)
		if err != nil {
			return fmt.Errorf("sbatch: error: --%s: %w", name, err)
		}
		opt.mem, opt.memKind = mem, name
	case "tmp":
		tmp, err := submit.ConvertMem(val)
		if err != nil {
			return fmt.Errorf("sbatch: error: --tmp: %w", err)
		}
		opt.tmp = tmp
	case "licenses":
		opt.licenses = val
	case "constraint":
		opt.constraint = val
	case "nodelist":
		opt.nodelist = val
	case "exclude":
		opt.exclude = val
	case "exclusive":
		opt.exclusive, opt.exclusiveMode = submit.ParseExclusive(val)
	case "begin":
		opt.begin = val
	case "mail-type":
		opt.mailType = val
	case "mail-user":
		opt.mailUser = val
	case "nice":
		// Bare --nice lowers the priority by 100, as SLURM does.
		n := 100
		if val != "" {
			v, err := atoi()
			if err != nil {
				return err
			}
			n = v
		}
		opt.nice, opt.haveNice = n, true
	case "gpus", "gpus-per-node":
		n, err := submit.ParseGPUCount(val)
		if err != nil {
			return fmt.Errorf("sbatch: error: %w", err)
		}
		opt.gpus, opt.haveGPUs = n, true
	case "gpus-per-task":
		n, err := submit.ParseGPUCount(val)
		if err != nil {
			return fmt.Errorf("sbatch: error: %w", err)
		}
		opt.gpusPerTask, opt.haveGPUsPerTask = n, true
	case "gpu-bind":
		opt.gpuBind = val
	case "gres":
		if n, ok := submit.GresGPUCount(val); ok {
			opt.gpus, opt.haveGPUs = n, true
		}
	case "signal":
		opt.signalDelay, opt.haveSignal = parseSignalDelay(val), true
	case "dependency":
		// Last-wins, like every other flag here and like SLURM: directives are
		// folded before the command line (see sbatch.go), so an explicit
		// --dependency on the command line replaces the script's, rather than
		// accumulating a hold on both sets.
		opt.holdJIDs = dependencyIDs(val)
		opt.depSpec = val
	case "export":
		opt.exportSpec = val
	case "open-mode", "wckey":
		// Accepted, no GE equivalent: GE appends output by default and has no
		// workload-characterization key.
	}
	return nil
}

// nonGPUGres returns the entries of a --gres value that are not gpu requests
// ("license:2" in "gpu:2,license:2"), so they are warned rather than dropped.
func nonGPUGres(val string) []string {
	var out []string
	for _, entry := range strings.Split(val, ",") {
		entry = strings.TrimSpace(entry)
		if entry != "" && entry != "gpu" && !strings.HasPrefix(entry, "gpu:") {
			out = append(out, entry)
		}
	}
	return out
}

// parseSignalDelay extracts the lead time (seconds) from a --signal value such
// as "USR2@90", "B:USR2@90", or "USR2" (no lead time -> 0).
func parseSignalDelay(val string) int {
	if at := strings.LastIndexByte(val, '@'); at >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(val[at+1:])); err == nil {
			return n
		}
	}
	return 0
}

// dependencyIDs collects the numeric predecessor job ids from a SLURM
// --dependency value ("afterok:12:13,afterany:14"). GE's -hold_jid waits for all
// listed jobs to finish regardless of exit status, so every after* form maps to
// the same hold. Separators are SLURM's own: ',' (AND), ':' (id list) and '?'
// (OR) -- omitting '?' silently swallowed the id fused to it, e.g.
// "afterok:11?afterany:12" held on 12 alone.
//
// What this CANNOT translate is reported by dependencyWarning, which is the
// authority on the gap; keep the two in step.
func dependencyIDs(val string) []string {
	var ids []string
	sep := func(r rune) bool { return r == ',' || r == ':' || r == '?' }
	for _, clause := range strings.FieldsFunc(val, sep) {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		if _, err := strconv.Atoi(clause); err == nil {
			ids = append(ids, clause)
		}
	}
	return ids
}

// dependencyKinds extracts the dependency type of each clause in a --dependency
// value, lowercased ("afterok:1?afterany:2" -> [afterok afterany]). Clauses are
// separated by ',' (AND) and '?' (OR); the type is whatever precedes the first
// ':'. Matching on whole types rather than substrings matters: every type starts
// with "after", so a Contains check on "after" would fire for all of them.
func dependencyKinds(val string) []string {
	var kinds []string
	for _, clause := range strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == '?' }) {
		kind, _, _ := strings.Cut(strings.TrimSpace(clause), ":")
		if kind != "" {
			kinds = append(kinds, strings.ToLower(kind))
		}
	}
	return kinds
}

// dependencyWarning reports what a --dependency request loses in translation, or
// "" when nothing is lost. GE has exactly one dependency primitive, -hold_jid,
// which releases when every predecessor FINISHES. Only SLURM's afterany means
// that; the rest lose something, worst first:
//
//   - No predecessor id at all (singleton, "after:99+10", an empty expansion).
//     Nothing is held and the job runs immediately -- strictly worse than an
//     approximated gate, so it must not be the quiet case.
//   - after is START-gated in SLURM ("after the specified jobs start"). Under
//     -hold_jid the job instead waits for the predecessor to END, which for the
//     usual case -- a client waiting on a long-lived server -- never happens.
//     This one can deadlock a workflow, so it is not "exact".
//   - Success-gated forms. afterok/aftercorr run anyway when the predecessor
//     fails, afternotok is inverted outright, and aftercorr additionally loses
//     its element-wise pairing (the whole array waits on the whole array).
func dependencyWarning(spec string, ids []string) string {
	if strings.TrimSpace(spec) == "" {
		return ""
	}
	if len(ids) == 0 {
		return "--dependency=" + spec + " has no job id Grid Engine can hold on: " +
			"the job is NOT held and runs immediately"
	}
	var startGated, successGated, inverted bool
	for _, k := range dependencyKinds(spec) {
		switch k {
		case "after":
			startGated = true
		case "afternotok":
			inverted = true
		case "afterok", "aftercorr":
			successGated = true
		}
	}
	switch {
	case inverted:
		return "--dependency=afternotok cannot be expressed: GE releases on completion " +
			"regardless of exit status, so the job runs whether or not the predecessor failed"
	case startGated:
		return "--dependency=after is start-gated in SLURM but completion-gated in Grid " +
			"Engine: the job waits for the predecessor to FINISH, not to start, which " +
			"never happens if the predecessor is long-lived. Use afterany if that is what " +
			"you meant"
	case successGated:
		return "--dependency=afterok/aftercorr is approximated: GE releases when the " +
			"predecessor finishes, whatever its exit status (and aftercorr's per-element " +
			"pairing becomes a whole-array hold)"
	}
	return ""
}

// parseArraySpec parses a SLURM --array value: "<min>-<max>[:<step>][%<throttle>]"
// or a single "<n>". Comma-separated lists (e.g. "0,2,4") are rejected because a
// GE array is a single contiguous -t range; submitit only ever emits the
// contiguous "0-{n-1}%p" form.
func parseArraySpec(val string) (min, max, step, throttle int, err error) {
	step = 1
	spec := strings.TrimSpace(val)
	if p := strings.IndexByte(spec, '%'); p >= 0 {
		t, e := strconv.Atoi(strings.TrimSpace(spec[p+1:]))
		if e != nil || t < 1 {
			return 0, 0, 0, 0, fmt.Errorf("sbatch: error: --array: invalid throttle in %q", val)
		}
		throttle = t
		spec = spec[:p]
	}
	if strings.ContainsRune(spec, ',') {
		return 0, 0, 0, 0, fmt.Errorf("sbatch: error: --array: comma-separated lists are not supported; use a contiguous <min>-<max> range")
	}
	if c := strings.IndexByte(spec, ':'); c >= 0 {
		s, e := strconv.Atoi(strings.TrimSpace(spec[c+1:]))
		if e != nil || s < 1 {
			return 0, 0, 0, 0, fmt.Errorf("sbatch: error: --array: invalid step in %q", val)
		}
		step = s
		spec = spec[:c]
	}
	lo, hi := spec, spec
	if d := strings.IndexByte(spec, '-'); d >= 0 {
		lo, hi = spec[:d], spec[d+1:]
	}
	a, e1 := strconv.Atoi(strings.TrimSpace(lo))
	b, e2 := strconv.Atoi(strings.TrimSpace(hi))
	if e1 != nil || e2 != nil || a < 0 || b < a {
		return 0, 0, 0, 0, fmt.Errorf("sbatch: error: --array: invalid range %q", val)
	}
	return a, b, step, throttle, nil
}
