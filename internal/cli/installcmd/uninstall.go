package installcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/install"
)

// RunUninstall is `slurm-shim uninstall`: it shows what undoing the install
// would change and, with --apply, does it. It either changes nothing (a
// refusal, a job still using what it would delete) or undoes the cluster side
// completely; only then, and only while nothing in the cluster points into the
// tree any more, does it edit the config and remove the files.
func RunUninstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("slurm-shim uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("apply", false, "perform the plan (default: print it and change nothing)")
	prefix := fs.String("prefix", "", "runtime tree to remove (default $SGE_ROOT/slurm-shim)")
	cfgFlag := fs.String("config", "", "config.yaml to clean up or purge (default: the cell path)")
	purge := fs.Bool("purge-config", false, "remove the config file instead of only dropping the partitions it routed to what is deleted")
	peName := fs.String("pe", "", "a PE install created under another name, for an install without a record")
	ignoreJobs := fs.Bool("ignore-jobs", false, "do not stop for jobs that still use the shim's PE, queue or complex")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *prefix == "" {
		root := os.Getenv("SGE_ROOT")
		if root == "" {
			fmt.Fprintln(stderr, "uninstall: error: SGE_ROOT is not set; source the cell's settings.sh or pass --prefix")
			return 2
		}
		*prefix = filepath.Join(root, "slurm-shim")
	}
	abs, err := filepath.Abs(*prefix)
	if err != nil {
		fmt.Fprintf(stderr, "uninstall: error: %v\n", err)
		return 2
	}
	spellings := []string{abs}
	if r, err := filepath.EvalSymlinks(abs); err == nil && r != abs {
		spellings = append(spellings, r)
	}

	st, err := install.ReadState(abs)
	if err != nil {
		fmt.Fprintf(stderr, "uninstall: error: %v\n", err)
		fmt.Fprintln(stderr, "uninstall: fix or remove the install record and re-run; nothing has been changed.")
		return 1
	}
	if st != nil && st.Prefix != "" && !sameTree(st.Prefix, abs) {
		fmt.Fprintf(stderr, "uninstall: error: the install record in %s is for %s; uninstall that prefix (nothing has been changed)\n",
			abs, st.Prefix)
		return 1
	}
	inTree := install.TreeMatcher(abs)

	admin, err := gedata.NewAdmin()
	if err != nil {
		fmt.Fprintf(stderr, "uninstall: error: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	facts, err := install.Discover(ctx, admin)
	if err != nil {
		fmt.Fprintf(stderr, "uninstall: error: %v (is qmaster up, and are you a manager?)\n", err)
		return 2
	}
	plan := install.PlanUninstall(facts, st, install.UninstallOptions{InTree: inTree, PEName: *peName})

	cfgPath := *cfgFlag
	if cfgPath == "" {
		cfgPath = config.CellPath()
	}
	treeErr := install.IsShimTree(abs)
	var files []string
	if treeErr == nil {
		files, _ = install.TreeFiles(abs)
	}
	record := "none (an install made before install recorded one: found by what points at the tree)"
	if st != nil {
		record = filepath.Join(abs, install.StateRel)
	}
	if treeErr != nil {
		fmt.Fprintf(stdout, "files      none removed: %v\n", treeErr)
	} else {
		fmt.Fprintf(stdout, "files      %s: %d installed file(s): %s\n", abs, len(files), strings.Join(files, " "))
	}
	fmt.Fprintf(stdout, "record     %s\n", record)
	if *purge {
		fmt.Fprintf(stdout, "config     %s  (removed: --purge-config)\n", cfgPath)
	} else {
		fmt.Fprintf(stdout, "config     %s  (kept; partitions on a deleted queue or PE are dropped)\n", cfgPath)
	}
	if st != nil && st.Config != "" && st.Config != cfgPath {
		fmt.Fprintf(stdout, "           install wrote %s; pass --config %s to act on that one\n", st.Config, st.Config)
	}
	fmt.Fprintln(stdout)
	if len(plan.Changes) == 0 {
		fmt.Fprintln(stdout, "  nothing in the cluster points at this install")
	}
	printPlan(stdout, plan)
	for _, w := range plan.Warnings {
		fmt.Fprintf(stdout, "  WARN      %s\n", w)
	}
	if len(plan.Refusals()) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "REFUSED rows above must be fixed first: uninstall changes nothing while any stands.")
		if *apply {
			return 1
		}
	}
	if !*apply {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Nothing was changed. Re-run with --apply to perform the plan.")
		return 0
	}

	if !*ignoreJobs {
		uses, err := gedata.JobUses(ctx, gedata.ExecRunner{})
		if err != nil {
			fmt.Fprintf(stderr, "uninstall: error: cannot list jobs (%v); nothing was changed (--ignore-jobs skips this check)\n", err)
			return 1
		}
		if busy := install.BusyJobs(plan, uses); len(busy) > 0 {
			fmt.Fprintf(stderr, "uninstall: error: job(s) %s, running or pending, still use the shim's PE, queue or complex; "+
				"nothing was changed -- wait for them, or qdel them, and re-run\n", strings.Join(busy, " "))
			return 1
		}
	}

	report := install.Apply(ctx, admin, plan)
	for _, o := range report.Outcomes {
		if o.Err != nil {
			fmt.Fprintf(stderr, "uninstall: error: %v\n", o.Err)
		}
	}
	fmt.Fprintf(stdout, "\ncluster    %d change(s) applied\n", report.Applied())
	if len(report.Failed()) > 0 {
		fmt.Fprintln(stderr, "uninstall: the files are kept so the install still works; fix the errors above and re-run")
		return 1
	}
	after, err := install.Discover(ctx, admin)
	if err != nil {
		fmt.Fprintf(stderr, "uninstall: error: re-reading the cluster: %v; the files are kept\n", err)
		return 1
	}
	if refs := install.References(after, inTree); len(refs) > 0 {
		fmt.Fprintf(stderr, "uninstall: the files are kept: the cluster still points into %s:\n  %s\n", abs, strings.Join(refs, "\n  "))
		return 1
	}

	if code := cleanConfig(cfgPath, *purge, plan, after, stdout, stderr); code != 0 {
		return code
	}
	if treeErr != nil {
		return 0
	}
	removed, err := install.RemoveTree(abs, install.ProfileDPath, spellings)
	fmt.Fprintf(stdout, "files      %d removed\n", len(removed))
	if err != nil {
		fmt.Fprintf(stderr, "uninstall: error: removing files: %v\n", err)
		return 1
	}
	return 0
}

// cleanConfig removes the config (--purge-config) or drops the partitions that
// route to a queue or PE the plan deleted, so the config left behind never
// sends a job to something that is gone. Purging the cell config is refused
// while another slurm-shim install remains in the cluster: every install reads
// that file by default.
func cleanConfig(path string, purge bool, plan install.Plan, after install.Facts, stdout, stderr io.Writer) int {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "uninstall: error: %v\n", err)
		return 1
	}
	if !fi.Mode().IsRegular() {
		fmt.Fprintf(stderr, "uninstall: error: %s is not a regular file; the config was not touched\n", path)
		return 1
	}
	if purge {
		if other := otherInstall(after); other != "" && path == config.CellPath() {
			fmt.Fprintf(stdout, "config     %s kept: another slurm-shim install may read it (%s)\n", path, other)
			return 0
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(stderr, "uninstall: error: removing %s: %v\n", path, err)
			return 1
		}
		fmt.Fprintf(stdout, "config     %s removed\n", path)
		return 0
	}

	deleted := map[string]bool{}
	for _, c := range plan.Changes {
		if c.Kind == install.ChangeDeleteQueue || c.Kind == install.ChangeDeletePE {
			deleted[c.Object] = true
		}
	}
	cfg, used, _, err := config.LoadFrom([]string{path})
	if err != nil || used == "" {
		return 0 // nothing readable to clean; install refuses such a file too
	}
	var gone, left []string
	for name, p := range cfg.Partitions {
		if deleted[p.Queue] || deleted[p.PE] {
			gone = append(gone, name)
		} else {
			left = append(left, name)
		}
	}
	if len(gone) == 0 {
		return 0
	}
	sort.Strings(gone)
	sort.Strings(left)
	newDefault := ""
	if len(left) > 0 {
		newDefault = left[0]
		if slices.Contains(left, "all") {
			newDefault = "all"
		}
	}
	prev, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "uninstall: error: %v\n", err)
		return 1
	}
	data, err := config.RemovePartitions(prev, gone, newDefault)
	if err == nil {
		err = writeConfigAtomic(path, data)
	}
	if err != nil {
		fmt.Fprintf(stderr, "uninstall: error: cleaning %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(stdout, "config     %s: dropped partition(s) %s\n", path, strings.Join(gone, " "))
	return 0
}

// otherInstall names an object that belongs to some slurm-shim install
// (its starter or slurm-shim-env), "" when there is none.
func otherInstall(f install.Facts) string {
	for _, q := range f.Queues {
		if filepath.Base(strings.Fields(q.StarterMethod + " x")[0]) == "slurm-shim-starter" {
			return "queue " + q.Name + " starter_method " + q.StarterMethod
		}
	}
	for _, pe := range f.PEs {
		if filepath.Base(strings.Fields(pe.StartProcArgs + " x")[0]) == "slurm-shim-env" {
			return "pe " + pe.Name + " start_proc_args " + pe.StartProcArgs
		}
	}
	return ""
}

// sameTree reports whether two prefixes name the same directory, lexically or
// after following symlinks.
func sameTree(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}
