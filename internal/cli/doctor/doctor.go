// Package doctor is `slurm-shim doctor`: one read-only report of everything
// support needs to know about a site -- versions, config, install tree, PE and
// queue wiring, memory complex, ports, spool exposure, scheduler health -- as
// PASS/WARN/FAIL lines to paste into a ticket. It consolidates checks the shim
// already performs piecemeal at submit and launch time.
package doctor

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/cli/ports"
	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/install"
	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
	"github.com/hpc-gridware/slurm-shim/internal/version"
)

// AccountingFixRelease and AccountingFixStamp name the OCS build that records a
// parallel job's exit_status in qacct; below it sacct reports every PE job as
// exit 0 (docs/solutions/integration-issues/pe-jobs-lose-exit-status-in-accounting.md).
const (
	AccountingFixRelease = "9.1.5"
	AccountingFixStamp   = "250826-0734"
)

// CompatNotes lists what a build older than the accounting fix cannot do.
func CompatNotes(b gedata.OCSBuild) []string {
	var notes []string
	if !b.AtLeast(AccountingFixRelease, AccountingFixStamp) {
		notes = append(notes,
			fmt.Sprintf("sacct ExitCode for parallel jobs is 0 regardless of outcome below OCS %s (%s); this is %s",
				AccountingFixRelease, AccountingFixStamp, b),
			"--nodes/--ntasks-per-node are not enforced (qsub -par needs OCS 9.1.5); the PE's allocation_rule places the nodes")
	}
	return notes
}

type report struct {
	w       io.Writer
	fails   int
	warns   int
	offline bool
}

func (r *report) pass(f string, a ...interface{}) { fmt.Fprintf(r.w, "PASS  "+f+"\n", a...) }
func (r *report) warn(f string, a ...interface{}) { r.warns++; fmt.Fprintf(r.w, "WARN  "+f+"\n", a...) }
func (r *report) fail(f string, a ...interface{}) { r.fails++; fmt.Fprintf(r.w, "FAIL  "+f+"\n", a...) }
func (r *report) info(f string, a ...interface{}) { fmt.Fprintf(r.w, "      "+f+"\n", a...) }
func (r *report) section(name string)             { fmt.Fprintf(r.w, "\n== %s\n", name) }

// Run is the entry point. Exit 0 with no FAIL lines, 1 otherwise.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("slurm-shim doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	offline := fs.Bool("offline", false, "skip everything that needs qmaster (package CI)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	r := &report{w: stdout, offline: *offline}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runner := gedata.ExecRunner{}

	fmt.Fprintf(stdout, "slurm-shim doctor  (paste this whole output into a support ticket)\n")

	// -- versions -------------------------------------------------------------
	r.section("versions")
	r.info("slurm-shim %s (reports as %s)", version.Shim, version.String(version.DefaultCompat))
	var build gedata.OCSBuild
	if b, err := gedata.OCSVersion(ctx, runner); err != nil {
		r.fail("OCS clients not reachable: %v (source $SGE_ROOT/$SGE_CELL/common/settings.sh?)", err)
	} else {
		build = b
		r.pass("OCS %s", b)
		for _, n := range CompatNotes(b) {
			r.warn("%s", n)
		}
	}
	sgeRoot, cell := os.Getenv("SGE_ROOT"), os.Getenv("SGE_CELL")
	if sgeRoot == "" {
		r.warn("SGE_ROOT is not set; cell-scoped config and install checks are skipped")
	}

	// -- config ---------------------------------------------------------------
	r.section("config")
	cfg, used, warns, err := config.LoadFrom(config.SearchPaths())
	switch {
	case err != nil:
		r.fail("config: %v", err)
		return 1
	case used == "":
		r.warn("no config file found (searched %s); compiled-in defaults apply", strings.Join(config.SearchPaths(), ", "))
	default:
		r.pass("config %s", used)
	}
	for _, w := range warns {
		r.warn("config: %s", w)
	}
	if len(cfg.Partitions) == 0 {
		r.fail("no partitions configured; sbatch -p has nothing to map to (run `slurm-shim install`)")
	}
	names := make([]string, 0, len(cfg.Partitions))
	for n := range cfg.Partitions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := cfg.Partitions[n]
		mark := ""
		if n == cfg.DefaultPartition {
			mark = " (default)"
		}
		r.info("partition %-12s queue %-12s pe %-12s slots %s%s", n, p.Queue, p.PE, p.Slots, mark)
	}

	// -- install tree ---------------------------------------------------------
	r.section("install tree")
	exe, _ := os.Executable()
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	prefix := filepath.Dir(filepath.Dir(exe))
	adminUser := ""
	if sgeRoot != "" {
		if u, err := install.AdminUser(sgeRoot, cell); err == nil {
			adminUser = u
		}
	}
	r.info("prefix %s (admin_user %s)", prefix, orRoot(adminUser))
	if probs := install.CheckTree(prefix, adminUser); len(probs) == 0 {
		r.pass("tree owned by root%s, not group/world-writable, all command links present", orAdmin(adminUser))
	} else {
		for _, p := range probs {
			r.fail("%s: %s", p.Path, p.Why)
		}
	}

	if *offline {
		r.section("skipped (--offline)")
		r.info("wiring, memory, spool, IJS, scheduler health need qmaster")
		return r.finish()
	}

	// -- wiring ---------------------------------------------------------------
	r.section("wiring")
	admin, err := gedata.NewAdmin()
	if err != nil {
		r.fail("%v", err)
		return r.finish()
	}
	seenPE := map[string]bool{}
	for _, n := range names {
		p := cfg.Partitions[n]
		q, err := admin.Queue(ctx, p.Queue)
		if err != nil {
			r.fail("partition %s: queue %s: %v", n, p.Queue, err)
			continue
		}
		wantStarter := install.StarterPath(prefix)
		switch q.StarterMethod {
		case wantStarter:
			r.pass("queue %s starter_method -> this tree", p.Queue)
		case "":
			r.fail("queue %s has no starter_method: jobs get no SLURM_* unless they source the hook (run `slurm-shim install`)", p.Queue)
		default:
			r.warn("queue %s starter_method is %s, not this tree's %s", p.Queue, q.StarterMethod, wantStarter)
		}
		if !containsStr(q.PEList, p.PE) {
			r.fail("queue %s does not offer pe %s (pe_list: %s)", p.Queue, p.PE, strings.Join(q.PEList, " "))
		}
		if seenPE[p.PE] {
			continue
		}
		seenPE[p.PE] = true
		pe, err := admin.PE(ctx, p.PE)
		if err != nil {
			r.fail("pe %s: %v", p.PE, err)
			continue
		}
		if !pe.ControlSlaves {
			r.fail("pe %s control_slaves FALSE: srun cannot launch steppers with qrsh -inherit", p.PE)
		}
		want := filepath.Join(prefix, "bin", "slurm-shim-env")
		switch pe.StartProcArgs {
		case want:
			r.pass("pe %s start_proc_args -> this tree, control_slaves TRUE", p.PE)
		default:
			r.fail("pe %s start_proc_args is %q, not this tree's slurm-shim-env", p.PE, pe.StartProcArgs)
		}
		pf := launch.Preflight(ctx, runner, p.PE)
		for _, e := range pf.Errors {
			r.fail("pe %s: %s", p.PE, e)
		}
		for _, w := range pf.Warnings {
			// The spool exposure is a cluster fact, reported once under security.
			if strings.Contains(w, "SI-51") {
				continue
			}
			r.warn("pe %s: %s", p.PE, w)
		}
	}
	if hosts, err := admin.ExecHosts(ctx); err == nil {
		r.info("%d exec host(s): %s -- each must have %s at the same path", len(hosts), strings.Join(hosts, " "), prefix)
	}

	// -- memory ---------------------------------------------------------------
	r.section("memory")
	if cfg.MemoryComplex == "" {
		r.info("memory_complex disabled; --mem is dropped")
	} else {
		scope, err := gedata.ConsumableScope(ctx, runner, cfg.MemoryComplex)
		if err != nil {
			r.fail("memory_complex %s: %v", cfg.MemoryComplex, err)
		} else {
			r.pass("memory_complex %s (consumable %s): --mem %s", cfg.MemoryComplex, scope, memSemantics(scope))
		}
		if w := submit.MemoryComplexWarning(cfg, submit.Request{Mem: "1G", HaveGPUs: true, GPUs: 1}); w != "" {
			r.warn("%s", w)
		}
	}

	// -- network --------------------------------------------------------------
	r.section("network")
	r.info("control channel TCP %d-%d, rendezvous TCP %d-%d, both inbound to a job's master node (`slurm-shim ports` prints the rules)",
		cfg.ControlPortBase, cfg.ControlPortBase+cfg.ControlPortRange-1,
		cfg.MasterPortBase, cfg.MasterPortBase+cfg.MasterPortRange-1)
	if cfg.ControlPortBase == 0 {
		r.warn("control_port_base is 0: srun binds an ephemeral port no firewall rule can describe")
	}

	// -- security -------------------------------------------------------------
	r.section("security")
	if w := launch.TokenSpoolWarning(ctx, runner); w != "" {
		r.warn("%s", w)
	} else {
		r.pass("execd spool is not traversable by other users (step tokens stay private)")
	}
	if daemon := rshDaemon(ctx, runner); daemon != "" && !strings.EqualFold(daemon, "builtin") {
		r.warn("rsh_daemon is %q, not builtin: srun --pty sessions get the environment but may have no terminal", daemon)
	} else if daemon != "" {
		r.pass("rsh_daemon builtin")
	}

	// -- scheduler ------------------------------------------------------------
	r.section("scheduler")
	if insts, err := gedata.QueueInstances(ctx, runner); err != nil {
		r.fail("qstat -f: %v", err)
	} else {
		bad := 0
		for _, qi := range insts {
			if strings.ContainsAny(qi.States, "Eau") {
				bad++
				r.warn("%s state %s", qi.Name, qi.States)
			}
		}
		if bad == 0 {
			r.pass("%d queue instance(s), none in error/alarm/unreachable state", len(insts))
		}
	}
	_ = build
	_ = ports.Run
	return r.finish()
}

func (r *report) finish() int {
	fmt.Fprintf(r.w, "\n%d FAIL, %d WARN\n", r.fails, r.warns)
	if r.fails > 0 {
		return 1
	}
	return 0
}

func memSemantics(scope string) string {
	if strings.EqualFold(scope, gedata.ConsumablePerSlot) {
		return "reserves memory per slot"
	}
	return "filters hosts on free memory but is not enforced"
}

// rshDaemon reads the cluster's rsh_daemon from qconf -sconf, "" if unknown.
func rshDaemon(ctx context.Context, r gedata.Runner) string {
	out, _, exit, err := r.Run(ctx, "qconf", "-sconf")
	if err != nil || exit != 0 {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "rsh_daemon" {
			return f[1]
		}
	}
	return ""
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func orRoot(u string) string {
	if u == "" {
		return "none (root)"
	}
	return u
}

func orAdmin(u string) string {
	if u == "" {
		return ""
	}
	return " or " + u
}
