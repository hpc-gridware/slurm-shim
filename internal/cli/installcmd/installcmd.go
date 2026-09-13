// Package installcmd is `slurm-shim install`: it shows what configuring this
// cluster for the shim would change, and with --apply does it. It never asks a
// question; defaults are chosen and printed with the reason.
package installcmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/cli/ports"
	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/install"
	"github.com/hpc-gridware/slurm-shim/internal/version"
)

// Run is the entry point.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("slurm-shim install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("apply", false, "perform the plan (default: print it and change nothing)")
	prefix := fs.String("prefix", "", "runtime tree (default $SGE_ROOT/slurm-shim)")
	from := fs.String("from", "", "payload to install from (default: the tree this binary runs from)")
	peName := fs.String("pe", install.DefaultPEName, "parallel environment to create or, with --force, repair")
	var queues multi
	fs.Var(&queues, "queue", "wire only this cluster queue (repeatable; default all)")
	force := fs.Bool("force", false, "overwrite an existing PE's start_proc_args or a queue's starter_method")
	expose := fs.String("expose", "none", "put the commands on PATH: none, module, profile.d")
	verify := fs.Bool("verify", false, "after --apply, submit a 2-node smoke job and check it")
	cfgPath := fs.String("config", "", "config.yaml to write (default: the cell path)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	sgeRoot := os.Getenv("SGE_ROOT")
	if *prefix == "" {
		if sgeRoot == "" {
			fmt.Fprintln(stderr, "install: error: SGE_ROOT is not set; source the cell's settings.sh or pass --prefix")
			return 2
		}
		*prefix = filepath.Join(sgeRoot, "slurm-shim")
	}
	if *cfgPath == "" {
		*cfgPath = config.CellPath()
	}
	if *from == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(stderr, "install: error: locating this binary: %v\n", err)
			return 2
		}
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		*from = filepath.Dir(filepath.Dir(exe)) // <payload>/bin/slurm-shim -> <payload>
	}

	admin, err := gedata.NewAdmin()
	if err != nil {
		fmt.Fprintf(stderr, "install: error: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	build, err := gedata.OCSVersion(ctx, gedata.ExecRunner{})
	if err != nil {
		fmt.Fprintf(stderr, "install: error: cannot reach the scheduler's clients: %v\n", err)
		return 2
	}
	facts, err := install.Discover(ctx, admin)
	if err != nil {
		fmt.Fprintf(stderr, "install: error: %v (is qmaster up, and are you a manager?)\n", err)
		return 2
	}
	plan := install.MakePlan(facts, install.Options{Prefix: *prefix, PEName: *peName, Queues: queues, Force: *force})

	// Fail on an unparseable config HERE, before any cluster change is applied.
	// The write below merges into this file rather than overwriting it, so it
	// refuses a file it cannot parse -- and refusing at write time would leave
	// the cluster already modified with no config to match. Better to stop
	// before touching anything.
	existing, cfgUsed, _, cfgErr := config.LoadFrom([]string{*cfgPath})
	if cfgErr != nil {
		fmt.Fprintf(stderr, "install: error: %s is not valid YAML: %v\n", *cfgPath, cfgErr)
		fmt.Fprintf(stderr, "install: fix or move it and re-run; nothing has been changed.\n")
		return 1
	}
	if cfgUsed == "" {
		existing = nil
	}
	cfg := install.GenerateConfig(plan, existing)

	adminUser, _ := install.AdminUser(sgeRoot, os.Getenv("SGE_CELL"))

	fmt.Fprintf(stdout, "slurm-shim %s on %s\n\n", version.Shim, build)
	fmt.Fprintf(stdout, "files      %s  <-  %s\n", *prefix, *from)
	fmt.Fprintf(stdout, "config     %s  (%s)\n", *cfgPath, mergeWord(cfgUsed))
	fmt.Fprintf(stdout, "partitions %s  (default %s)\n", partitionList(plan), plan.DefaultPartition)
	if plan.GPUComplex != "" {
		fmt.Fprintf(stdout, "gpu        RSMAP complex %q -> gpu.gres_complex\n", plan.GPUComplex)
	}
	fmt.Fprintln(stdout)
	printPlan(stdout, plan)

	if len(plan.Refusals()) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "REFUSED changes above are not applied; re-run with --force to override, or wire them by hand.")
	}
	if !*apply {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Nothing was changed. Re-run with --apply to perform the plan.")
		return 0
	}

	// Apply: files first (so start_proc_args points at something that exists),
	// then the cluster, then the config.
	fmt.Fprintln(stdout)
	if err := install.InstallTree(*from, *prefix); err != nil {
		fmt.Fprintf(stderr, "install: error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "installed  %s\n", *prefix)
	for _, p := range install.CheckTree(*prefix, adminUser) {
		fmt.Fprintf(stderr, "install: warning: %s: %s\n", p.Path, p.Why)
	}
	if path, err := install.Expose(*prefix, install.ExposeMode(*expose), version.Shim); err != nil {
		fmt.Fprintf(stderr, "install: error: %v\n", err)
		return 1
	} else if path != "" {
		fmt.Fprintf(stdout, "exposed    %s\n", path)
		if install.ExposeMode(*expose) == install.ExposeModule {
			fmt.Fprintf(stdout, "           add %s to MODULEPATH, then: module load slurm-shim\n",
				filepath.Join(*prefix, "share", "modulefiles"))
		}
	}

	report := install.Apply(ctx, admin, plan)
	for _, o := range report.Outcomes {
		if o.Err != nil {
			fmt.Fprintf(stderr, "install: error: %v\n", o.Err)
		}
	}
	fmt.Fprintf(stdout, "cluster    %d change(s) applied\n", report.Applied())

	// MERGE, never overwrite. Rendering the struct over the file drops every key
	// the running binary does not model -- which silently loses site settings on
	// an upgrade, a rollback, or an install run from an older binary. See
	// config.MergeInto.
	prev, readErr := os.ReadFile(*cfgPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		fmt.Fprintf(stderr, "install: error: reading %s: %v\n", *cfgPath, readErr)
		return 1
	}
	data, err := config.MergeInto(prev, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "install: error: rendering config: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "install: error: %v\n", err)
		return 1
	}
	if err := writeConfigAtomic(*cfgPath, data); err != nil {
		fmt.Fprintf(stderr, "install: error: writing %s: %v\n", *cfgPath, err)
		return 1
	}
	fmt.Fprintf(stdout, "config     %s written\n", *cfgPath)

	fmt.Fprintln(stdout)
	ports.Run(cfg, stdout)

	if len(report.Failed()) > 0 {
		return 1
	}
	if *verify {
		fmt.Fprintln(stdout)
		return runVerify(ctx, cfg, plan, stdout, stderr)
	}
	return 0
}

// printPlan renders the changes one per line, old -> new.
func printPlan(w io.Writer, p install.Plan) {
	for _, c := range p.Changes {
		switch c.Kind {
		case install.ChangeUnchanged:
			fmt.Fprintf(w, "  ok        %-12s %-16s %s\n", c.Object, c.Attr, c.New)
		case install.ChangeRefused:
			fmt.Fprintf(w, "  REFUSED   %-12s %-16s %s -> %s\n            %s\n", c.Object, c.Attr, orNone(c.Old), c.New, c.Reason)
		case install.ChangeAddToPEList:
			fmt.Fprintf(w, "  change    %-12s %-16s %s += %s\n", c.Object, c.Attr, orNone(c.Old), c.New)
		case install.ChangeAddPE:
			fmt.Fprintf(w, "  add       pe %-9s start_proc_args %s, control_slaves TRUE, allocation_rule %s\n",
				c.Object, p.PE.StartProcArgs, p.PE.AllocationRule)
		default:
			fmt.Fprintf(w, "  change    %-12s %-16s %s -> %s\n", c.Object, c.Attr, orNone(c.Old), c.New)
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "NONE"
	}
	return s
}

func mergeWord(used string) string {
	if used == "" {
		return "new"
	}
	return "merge into existing"
}

func partitionList(p install.Plan) string {
	var names []string
	for _, part := range p.Partitions {
		names = append(names, part.Name+"="+part.Queue)
	}
	return strings.Join(names, " ")
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// writeConfigAtomic replaces the config file by rename, preserving the mode of
// the file it replaces.
//
// os.WriteFile truncates first and writes second, so an interruption between
// the two leaves a truncated config -- the same data loss MergeInto exists to
// prevent, arriving by a different door. This is not theoretical here: the
// config lives under $SGE_ROOT/$SGE_CELL/common, which on a normal cluster is
// an NFS export, and a write to it can fail partway. A rename is atomic, so the
// old config survives intact until the new one is complete on disk.
//
// The mode is carried over because a site that restricted its config to 0600
// should not have it widened to world-readable by an upgrade. New files get
// 0644: every node reads this file, so it must stay readable by the users
// running jobs.
func writeConfigAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	dir := filepath.Dir(path)
	// Same directory, so the rename cannot cross a filesystem boundary.
	tmp, err := os.CreateTemp(dir, ".slurm-shim-config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup of the temp file, and a no-op once the rename has
	// succeeded. Discarded deliberately: there is no useful recovery from a
	// failed unlink of a file we are abandoning anyway, and reporting it would
	// mask the real error on the paths below.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		// Close errors are discarded on the FAILURE paths only: we already hold
		// the error that matters, and returning the close error instead would
		// hide why the write failed. The success path below checks Close, which
		// is where a deferred flush actually surfaces.
		_ = tmp.Close()
		return err
	}
	// fsync before rename: without it the rename can land while the contents are
	// still only in the page cache, which a crash then loses.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; set the intended mode before it is visible
	// under its real name.
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
