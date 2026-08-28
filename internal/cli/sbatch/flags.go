package sbatch

import (
	"fmt"
	"strconv"
	"strings"
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

	script     string   // script file path (first non-flag token)
	scriptArgs []string // tokens after the script
}

// knownLong is the set of long flags the translator handles. Every one of them
// takes a value -- parseFlags consumes the next token when the "=" form was not
// used -- so the bool is only there to make it a set; it is never read. Anything
// absent is warn-and-ignored (REQ-SBT-001).
var knownLong = map[string]bool{
	"nodes": true, "ntasks": true, "ntasks-per-node": true, "cpus-per-task": true,
	"partition": true, "job-name": true, "output": true, "error": true,
	"chdir": true, "wrap": true, "array": true,
	"time": true, "mem": true, "mem-per-cpu": true,
	"gpus": true, "gpus-per-node": true, "gres": true,
	"gpus-per-task": true, "gpu-bind": true,
	"signal": true, "dependency": true, "export": true,
	// Accepted and intentionally ignored (GE has no distinct behavior to map):
	"open-mode": true, "wckey": true,
}

// unmappableLong are flags the shim recognizes but deliberately does not
// translate, mapped to the reason. They are still ignored; this only upgrades
// the generic "unknown directive" warning to one that names the Grid Engine
// equivalent. Keep them out of knownLong -- these are booleans, and everything
// in that set consumes a value, which here would be the script path.
var unmappableLong = map[string]string{
	"exclusive": "--exclusive is not translated: Grid Engine expresses whole-node " +
		"allocation through the parallel environment, not the job. Use a partition " +
		"whose PE has allocation_rule $pe_slots sized to the node, or add an " +
		"exclusive complex -- both are site configuration, not a submission flag",
}

var shortToLong = map[byte]string{
	'N': "nodes", 'n': "ntasks", 'c': "cpus-per-task", 'p': "partition",
	'J': "job-name", 'o': "output", 'e': "error", 'D': "chdir",
	'a': "array", 't': "time", 'd': "dependency",
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

// parseFlags folds option tokens (from #SBATCH directives and/or the command
// line) into options, returning warnings for unknown flags. The first non-flag
// token is the job script; the rest are its arguments. Command-line tokens
// should follow directive tokens so the command line wins on conflicts.
func parseFlags(tokens []string) (options, []string, error) {
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

	for ; i < len(tokens); i++ {
		tok := tokens[i]
		switch {
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
					if why, ok := unmappableLong[name]; ok {
						warns = append(warns, why)
					} else {
						warns = append(warns, "unknown directive --"+name+" ignored")
					}
					warned[name] = true
				}
				// Known value-taking Pyxis flags consume their space-form value so
				// it is not mistaken for the script; boolean flags consume nothing.
				// Other unknown space-form flags cannot have their arity inferred
				// without the SLURM flag table (REQ-RUN-005, deferred) - clearml
				// templates use the = form, avoiding the ambiguity.
				if !hasVal && containerValueFlags[name] {
					_, _ = next()
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
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			flag := tok[1]
			long, ok := shortToLong[flag]
			if !ok {
				if !warned[string(flag)] {
					warns = append(warns, "unknown directive -"+string(flag)+" ignored")
					warned[string(flag)] = true
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
	case "partition":
		opt.partition = val
	case "job-name":
		opt.jobName = val
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
		sec, err := parseSlurmTime(val)
		if err != nil {
			return err
		}
		opt.timeSec, opt.haveTime = sec, true
	case "mem", "mem-per-cpu":
		opt.mem = convertMem(val)
	case "gpus", "gpus-per-node":
		n, err := parseGPUCount(val)
		if err != nil {
			return err
		}
		opt.gpus, opt.haveGPUs = n, true
	case "gpus-per-task":
		n, err := parseGPUCount(val)
		if err != nil {
			return err
		}
		opt.gpusPerTask, opt.haveGPUsPerTask = n, true
	case "gpu-bind":
		opt.gpuBind = val
	case "gres":
		if n, ok := gresGPUCount(val); ok {
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

// parseSlurmTime converts a SLURM --time value to seconds. It accepts plain
// minutes ("60"), "MM:SS", "HH:MM:SS", and the day forms "D-HH", "D-HH:MM",
// "D-HH:MM:SS".
func parseSlurmTime(val string) (int, error) {
	val = strings.TrimSpace(val)
	days := 0
	hasDays := false
	if d := strings.IndexByte(val, '-'); d >= 0 {
		n, err := strconv.Atoi(val[:d])
		if err != nil {
			return 0, fmt.Errorf("sbatch: error: --time: invalid days in %q", val)
		}
		days, val, hasDays = n, val[d+1:], true
	}
	parts := strings.Split(val, ":")
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return 0, fmt.Errorf("sbatch: error: --time: invalid value %q", val)
		}
		nums[i] = n
	}
	var h, m, s int
	switch {
	case hasDays: // D-HH[:MM[:SS]] -- keyed on the dash, not days>0, so "0-12:30" works
		h = nums[0]
		if len(nums) > 1 {
			m = nums[1]
		}
		if len(nums) > 2 {
			s = nums[2]
		}
	case len(nums) == 1: // minutes
		m = nums[0]
	case len(nums) == 2: // MM:SS
		m, s = nums[0], nums[1]
	default: // HH:MM:SS
		h, m, s = nums[0], nums[1], nums[2]
	}
	return days*86400 + h*3600 + m*60 + s, nil
}

// convertMem rewrites a SLURM memory value ("4GB", "512MB", "1024") into GE's
// single-letter suffix form ("4G", "512M", "1024M"). SLURM's default unit is
// megabytes, so a bare number gains an M.
func convertMem(val string) string {
	v := strings.TrimSpace(val)
	if v == "" {
		return ""
	}
	up := strings.ToUpper(v)
	for _, suf := range []struct{ slurm, ge string }{
		{"KB", "K"}, {"MB", "M"}, {"GB", "G"}, {"TB", "T"},
	} {
		if strings.HasSuffix(up, suf.slurm) {
			return strings.TrimSpace(up[:len(up)-2]) + suf.ge
		}
	}
	last := up[len(up)-1]
	if last == 'K' || last == 'M' || last == 'G' || last == 'T' {
		return up
	}
	return up + "M" // bare number: SLURM default unit is MB
}

func parseGPUCount(val string) (int, error) {
	// --gpus may be "N" or "type:N"; take the trailing count.
	v := val
	if c := strings.LastIndexByte(v, ':'); c >= 0 {
		v = v[c+1:]
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("sbatch: error: --gpus: invalid count %q", val)
	}
	return n, nil
}

// gresGPUCount extracts the gpu count from a --gres value like "gpu:2" or
// "gpu:a100:2"; it returns ok=false for a non-gpu gres.
func gresGPUCount(val string) (int, bool) {
	for _, entry := range strings.Split(val, ",") {
		entry = strings.TrimSpace(entry)
		if !strings.HasPrefix(entry, "gpu:") && entry != "gpu" {
			continue
		}
		fields := strings.Split(entry, ":")
		if n, err := strconv.Atoi(fields[len(fields)-1]); err == nil && n >= 0 {
			return n, true
		}
		return 1, true // "gpu" or "gpu:type" without a count means one
	}
	return 0, false
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
