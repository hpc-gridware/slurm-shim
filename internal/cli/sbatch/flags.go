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

	script     string   // script file path (first non-flag token)
	scriptArgs []string // tokens after the script
}

// longVal maps a long flag to the option setter; the bool is whether it takes a
// value. Only the flags the translator needs are known; everything else is
// warn-and-ignore (REQ-SBT-001).
var knownLong = map[string]bool{
	"nodes": true, "ntasks": true, "ntasks-per-node": true, "cpus-per-task": true,
	"partition": true, "job-name": true, "output": true, "error": true,
	"chdir": true, "wrap": true,
}

var shortToLong = map[byte]string{
	'N': "nodes", 'n': "ntasks", 'c': "cpus-per-task", 'p': "partition",
	'J': "job-name", 'o': "output", 'e': "error", 'D': "chdir",
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
					warns = append(warns, "unknown directive --"+name+" ignored")
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
	}
	return nil
}
