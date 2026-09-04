// Package env implements the slurm-shim-env fabricator command (spec section 6).
// It runs the fabricator and emits the SLURM environment contract either to the
// per-job state files (PE mode) or to stdout (wrapper mode, --export).
package env

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/fabricator"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// Run executes the fabricator command and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	exportMode := hasFlag(args, "--export")
	if hasFlag(args, "--cleanup") {
		return cleanup(stderr)
	}

	// PE mode writes into the per-job state dir, which sits at a predictable,
	// co-tenant-reachable path. EnsureStateDir is the trust boundary: it refuses a
	// TMPDIR that is not the job's own and evicts anything planted at the state
	// dir path. Wrapper mode prints to stdout and needs no state dir at all.
	var stateDir string
	if !exportMode {
		var err error
		stateDir, err = fabricator.EnsureStateDir(os.Getenv("TMPDIR"))
		if err != nil {
			fmt.Fprintf(stderr, "slurm-shim-env: error: %v\n", err)
			// There is nowhere trustworthy to leave a sentinel, so leave nothing:
			// the hook then sees "no fabrication" and warns, and exit 0 keeps the
			// queue instance out of E state (see fail).
			return fail(exportMode, "", stdout, stderr)
		}
	}

	cfg, warnings, err := config.Load()
	if err != nil {
		// Route through fail(), never a bare non-zero return. In PE mode this hook
		// IS start_proc_args: exiting non-zero puts the queue instance into E state
		// and disables it for every user on the host. In wrapper mode the job's
		// `eval "$(slurm-shim-env --export)"` needs the `exit 1` token, or it runs
		// on with no SLURM_* environment at all and silently produces wrong results.
		fmt.Fprintf(stderr, "slurm-shim-env: error: %v\n", err)
		return fail(exportMode, stateDir, stdout, stderr)
	}
	for _, w := range warnings {
		fmt.Fprintf(stderr, "slurm-shim-env: warning: %s\n", w)
	}

	res, err := fabricator.Fabricate(fabricator.Options{
		Env:      os.Getenv,
		Config:   cfg,
		Identity: gedata.RealIdentity{Runner: gedata.ExecRunner{}},
		// Runner drives qstat-based GPU/memory discovery (REQ-GPU-001, A27). In PE
		// mode this fabricator runs on the master with JOB_ID set, so it can query
		// the granted RSMAP; without it, GPU discovery falls back to the not-
		// multi-host-safe SGE_HGR path (SI-19).
		Runner:  gedata.ExecRunner{},
		NowUnix: time.Now().Unix(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "slurm-shim-env: error: %v\n", err)
		return fail(exportMode, stateDir, stdout, stderr)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(stderr, "slurm-shim-env: warning: %s\n", w)
	}

	// Put the shim's own command symlinks (srun, sbatch, ...) on the job's PATH.
	// GE runs batch/PE scripts in non-login shells, so /etc/profile.d is never
	// sourced and the shim bin dir would otherwise be missing from PATH.
	if exe, err := os.Executable(); err == nil {
		res.ShimBinDir = filepath.Dir(exe)
	}

	if exportMode {
		fmt.Fprint(stdout, res.RenderExports())
		return 0
	}

	if err := fabricator.WriteState(stateDir, res); err != nil {
		fmt.Fprintf(stderr, "slurm-shim-env: error: %v\n", err)
		return fail(exportMode, stateDir, stdout, stderr)
	}
	if err := layout.InitStepCounter(filepath.Join(stateDir, layout.StepCtrFile)); err != nil {
		fmt.Fprintf(stderr, "slurm-shim-env: error: %v\n", err)
		return fail(exportMode, stateDir, stdout, stderr)
	}
	return 0
}

// fail applies the mode-specific failure contract: wrapper mode prints the sole
// stdout token `exit 1` so `eval` aborts the job (REQ-FAB-008); PE mode writes
// the sentinel and exits 0 so a failed start_proc_args does not error the queue
// (REQ-FAB-009). An empty stateDir means no trustworthy state dir exists: write
// nothing rather than plant a sentinel where a co-tenant could have put one.
func fail(exportMode bool, stateDir string, stdout, stderr io.Writer) int {
	if exportMode {
		fmt.Fprintln(stdout, "exit 1")
		return 1
	}
	if stateDir == "" {
		return 0
	}
	if err := fabricator.WriteSentinel(stateDir); err != nil {
		fmt.Fprintf(stderr, "slurm-shim-env: error: writing sentinel: %v\n", err)
	}
	return 0
}

// cleanup removes the state dir. With TMPDIR unset there is nothing of ours to
// remove: the fabricator never writes to a shared fallback path.
func cleanup(stderr io.Writer) int {
	tmp := os.Getenv("TMPDIR")
	if tmp == "" {
		return 0
	}
	if err := os.RemoveAll(fabricator.StateDir(tmp)); err != nil {
		fmt.Fprintf(stderr, "slurm-shim-env: warning: cleanup: %v\n", err)
	}
	return 0
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
