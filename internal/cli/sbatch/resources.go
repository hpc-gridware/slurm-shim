package sbatch

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

// expandHosts reads the -w and -x hostlists; a -w list also sizes the job
// when --nodes was not given (submit.NodesForList).
func expandHosts(opt *options) ([]string, []string, error) {
	nodelist, err := submit.ExpandHosts("--nodelist", opt.nodelist)
	if err != nil {
		return nil, nil, fmt.Errorf("sbatch: error: %w", err)
	}
	exclude, err := submit.ExpandHosts("--exclude", opt.exclude)
	if err != nil {
		return nil, nil, fmt.Errorf("sbatch: error: %w", err)
	}
	nodes, err := submit.NodesForList(opt.nodes, nodelist, exclude)
	if err != nil {
		return nil, nil, fmt.Errorf("sbatch: error: %w", err)
	}
	opt.nodes = nodes
	return nodelist, exclude, nil
}

// resolveResources resolves licenses, features, --exclusive, host lists,
// --tmp and a per-CPU or per-GPU memory request (submit.Resolve). It rewrites
// opt.mem to the value the memory complex takes and returns the extra -l
// entries, whether -w e can still judge the job, and the warnings to print.
func resolveResources(runner gedata.Runner, cfg *config.Config, opt *options, nodelist, exclude []string,
	slots int, rule allocationRule) (submit.Grant, error) {
	res := submit.Resources{
		Licenses: opt.licenses, Constraint: opt.constraint,
		Exclusive: opt.exclusive, ExclusiveMode: opt.exclusiveMode,
		Nodelist: nodelist, Exclude: exclude, Tmp: opt.tmp,
	}
	nodes := rule.Nodes
	if nodes < 1 {
		nodes = opt.nodes
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.QstatTimeout.Duration)
	defer cancel()
	req := opt.request()
	g, err := submit.Resolve(ctx, runner, cfg, res, &req, opt.memKind, slots, nodes)
	if err != nil {
		return submit.Grant{}, fmt.Errorf("sbatch: error: %w", err)
	}
	opt.mem = req.Mem
	return g, nil
}

// controlArgs maps the job-control options onto qsub: -h (held), -a (start
// time), -r (rerunnable), -m/-M (mail) and -p (priority).
func controlArgs(opt options, now time.Time) ([]string, []string, error) {
	var args, warns []string
	if opt.hold {
		args = append(args, "-h")
	}
	if opt.begin != "" {
		t, err := submit.ParseBegin(opt.begin, now)
		if err != nil {
			return nil, nil, fmt.Errorf("sbatch: error: %w", err)
		}
		args = append(args, "-a", submit.BeginStamp(t))
	}
	switch {
	case opt.requeue != "":
		args = append(args, "-r", opt.requeue)
	case opt.haveSignal:
		// A --signal job is checkpointed then requeued (submitit), so it must be
		// rerunnable; an explicit --no-requeue above wins.
		args = append(args, "-r", "y")
	}
	if opt.mailType != "" {
		m, w := mailFlags(opt.mailType)
		args = append(args, "-m", m)
		warns = appendWarn(warns, w)
	}
	if opt.mailUser != "" {
		args = append(args, "-M", opt.mailUser)
	}
	if opt.haveNice {
		// SLURM's nice lowers the priority as it grows; Grid Engine's -p raises
		// it, within -1023..1024, and only managers may go above 0.
		args = append(args, "-p", strconv.Itoa(min(max(-opt.nice, -1023), 1024)))
	}
	return args, warns, nil
}

// mailFlags maps --mail-type onto qsub -m letters: b(egin), e(nd) and
// a(bort, which Grid Engine also sends on a reschedule). The SLURM types
// without a Grid Engine event are named in the warning.
func mailFlags(spec string) (string, string) {
	have := map[byte]bool{}
	var lost []string
	none := false
	for _, t := range strings.Split(strings.ToUpper(spec), ",") {
		switch t = strings.TrimSpace(t); t {
		case "NONE":
			none = true
		case "BEGIN":
			have['b'] = true
		case "END":
			have['e'] = true
		case "FAIL", "REQUEUE":
			have['a'] = true
		case "ALL":
			have['b'], have['e'], have['a'] = true, true, true
		case "", "INVALID_DEPEND", "STAGE_OUT", "ARRAY_TASKS":
			// No separate event: a broken dependency and stage-out do not exist
			// in Grid Engine, and it mails per array task already.
		default:
			lost = append(lost, t)
		}
	}
	var b strings.Builder
	for _, c := range []byte("bea") {
		if have[c] && !none {
			b.WriteByte(c)
		}
	}
	m := b.String()
	if m == "" {
		m = "n"
	}
	warn := ""
	if len(lost) > 0 {
		warn = "--mail-type " + strings.Join(lost, ",") + " ignored: Grid Engine mails on begin, end and abort only"
	}
	return m, warn
}
