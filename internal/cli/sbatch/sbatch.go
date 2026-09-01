package sbatch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// Run is the sbatch entry point. The runner is wrapped for dry-run mode so a
// mutating client cannot be reached by a path that skipped the explicit branch;
// the branch itself is what renders the report.
func Run(args []string, stdout, stderr io.Writer) int {
	cfg, _, _ := config.Load()
	self, _ := os.Executable()
	return run(dryrun.Wrap(gedata.ExecRunner{}, stderr, "sbatch"), cfg, self, args, stdout, stderr)
}

// baseName is filepath.Base, named so the dry-run report can show the shim's
// command name without importing path handling into its own file.
func baseName(p string) string { return filepath.Base(p) }

// run parses directives + CLI flags, translates to a qsub submission, runs
// `qsub -terse`, and prints "Submitted batch job <id>" (spec sec. 7.6).
func run(runner gedata.Runner, cfg *config.Config, self string, args []string, stdout, stderr io.Writer) int {
	// Pre-scan the command line to locate the script whose directives we read.
	pre, _, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}

	var tokens []string
	if pre.script != "" && pre.wrap == "" {
		scriptBytes, err := os.ReadFile(pre.script)
		if err != nil {
			fmt.Fprintf(stderr, "sbatch: error: cannot read script %q: %v\n", pre.script, err)
			return 1
		}
		tokens = ParseDirectives(scriptBytes)
	}
	// Directive flags first, command line second so the command line wins.
	tokens = append(tokens, args...)

	opt, warns, err := parseFlags(tokens)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	for _, w := range warns {
		fmt.Fprintln(stderr, "sbatch: warning: "+w)
	}

	if opt.script == "" && opt.wrap == "" {
		fmt.Fprintln(stderr, "sbatch: error: no script or --wrap command given")
		return 1
	}
	if opt.partition == "" {
		opt.partition = cfg.DefaultPartition // SLURM's DEFAULT-partition behavior
	}
	if opt.partition == "" {
		fmt.Fprintln(stderr, "sbatch: error: no partition specified (and no default_partition configured)")
		return 1
	}
	part, ok := cfg.Partitions[opt.partition]
	if !ok {
		fmt.Fprintf(stderr, "sbatch: error: unknown partition %q\n", opt.partition)
		return 1
	}

	if err := validateGeometry(opt); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}

	slots, err := computeSlots(opt, part)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}

	qargs, qwarns := buildQsubArgs(cfg, opt, part, slots)
	for _, w := range qwarns {
		fmt.Fprintln(stderr, "sbatch: warning: "+w)
	}

	// Dry run reports and stops before any state changes -- including
	// materializeScript, whose wrapper mode would spool a copy of the script for
	// a job that is never submitted. --test-only is SLURM's spelling of the same
	// request and reaches callers that control only argv or #SBATCH directives.
	if dryrun.Enabled() || opt.testOnly {
		if v := dryrun.Unrecognized(); v != "" {
			fmt.Fprintf(stderr, "sbatch: warning: %s=%q is not a recognized on/off value; treating as off\n",
				dryrun.EnvVar, v)
		}
		return dryRun(runner, cfg, self, opt, part, slots, qargs, stdout, stderr)
	}
	if v := dryrun.Unrecognized(); v != "" {
		fmt.Fprintf(stderr, "sbatch: warning: %s=%q is not a recognized on/off value; submitting for real\n",
			dryrun.EnvVar, v)
	}

	scriptPath, cleanup, err := materializeScript(cfg, self, opt)
	if err != nil {
		fmt.Fprintf(stderr, "sbatch: error: %v\n", err)
		return 1
	}
	defer cleanup()

	qargs = append(qargs, scriptPath)
	qargs = append(qargs, opt.scriptArgs...)

	out, errOut, exit, err := runner.Run(context.Background(), "qsub", qargs...)
	if err != nil {
		fmt.Fprintf(stderr, "sbatch: error: running qsub: %v\n", err)
		return 1
	}
	if exit != 0 {
		if msg := trim(errOut); msg != "" {
			fmt.Fprintf(stderr, "sbatch: error: %s\n", msg)
		}
		return 1
	}
	id := parseJobID(string(out))
	if id == "" {
		fmt.Fprintln(stderr, "sbatch: error: qsub did not return a job id")
		return 1
	}
	fmt.Fprintf(stdout, "Submitted batch job %s\n", id)
	return 0
}

// submitMode names how the script reaches qsub.
type submitMode int

const (
	submitPlain   submitMode = iota // the user's file, submitted unchanged
	submitWrap                      // a temp script synthesized from --wrap
	submitWrapper                   // wrapper mode (SI-57): a generated wrapper execs the stored original
)

// submitShape is the decision materializeScript acts on and the dry run reports:
// which of the three shapes applies, where the spool directory is rooted, and the
// path pattern to show for a directory whose name is only chosen at submit time.
type submitShape struct {
	mode         submitMode
	spoolRoot    string
	spoolPattern string
}

// submitPlan resolves the submission shape without touching the filesystem. Both
// materializeScript (which acts on it) and the dry run (which reports it) read it
// here, so the reported script path cannot drift from the submitted one.
func submitPlan(cfg *config.Config, opt options) submitShape {
	if !cfg.WrapperMode {
		if opt.wrap == "" {
			return submitShape{mode: submitPlain}
		}
		return submitShape{
			mode:         submitWrap,
			spoolRoot:    os.TempDir(),
			spoolPattern: filepath.Join(os.TempDir(), "slurm-shim-sbatch-XXXX", "wrap.sh"),
		}
	}
	// Wrapper mode: the stored original is referenced by path at RUN time, so it
	// must live on shared storage that persists for the job (SI-57). Prefer the
	// configured spool dir; else store next to the user's script (assumed shared).
	root := cfg.WrapperSpoolDir
	if root == "" {
		if opt.wrap != "" {
			root = "." // --wrap has no script dir; submit CWD is usually shared
		} else {
			root = filepath.Dir(opt.script)
		}
	}
	return submitShape{
		mode:         submitWrapper,
		spoolRoot:    root,
		spoolPattern: filepath.Join(root, ".slurm-shim-sbatch-XXXX", "wrapper.sh"),
	}
}

// materializeScript returns the path to submit to qsub and a cleanup func. In PE
// (default) mode it submits the user script directly (a --wrap command becomes a
// throwaway temp script that qsub copies into GE's spool, so it is cleaned after
// submit). In wrapper mode (SI-57) it stores the original script verbatim and
// submits a shim wrapper that fabricates then execs it; that spool is NOT
// cleaned, because the wrapper references the stored original by path at run
// time - it must survive until the job runs (a shared-FS/retention concern the
// site owns, e.g. via stop_proc_args).
func materializeScript(cfg *config.Config, self string, opt options) (string, func(), error) {
	noop := func() {}
	shape := submitPlan(cfg, opt)

	// The "original" is the user's script file, or a script synthesized from
	// --wrap.
	var origBytes []byte
	origName := "orig.sh"
	if opt.wrap != "" {
		origBytes = []byte("#!/bin/bash\n" + opt.wrap + "\n")
	} else {
		b, err := os.ReadFile(opt.script)
		if err != nil {
			return "", noop, fmt.Errorf("reading script %q: %w", opt.script, err)
		}
		origBytes = b
		origName = "orig" + filepath.Ext(opt.script)
	}

	switch shape.mode {
	case submitPlain:
		return opt.script, noop, nil // submit the user's file unchanged
	case submitWrap:
		// A --wrap temp script is copied into GE's spool by qsub at submit time,
		// so it is safe to clean once qsub returns; node-local /tmp is fine.
		dir, err := os.MkdirTemp("", "slurm-shim-sbatch-")
		if err != nil {
			return "", noop, err
		}
		p := filepath.Join(dir, "wrap.sh")
		if err := os.WriteFile(p, origBytes, 0o700); err != nil {
			return "", noop, err
		}
		return p, func() { _ = os.RemoveAll(dir) }, nil
	}

	dir, err := os.MkdirTemp(shape.spoolRoot, ".slurm-shim-sbatch-")
	if err != nil {
		return "", noop, err
	}
	stored := filepath.Join(dir, origName)
	if err := os.WriteFile(stored, origBytes, 0o700); err != nil {
		return "", noop, err
	}
	wrapper := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(wrapper, []byte(buildWrapper(self, stored)), 0o700); err != nil {
		return "", noop, err
	}
	return wrapper, noop, nil
}

func trim(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
