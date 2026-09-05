// Command slurm-shim is the single static binary behind every shimmed SLURM
// command. It dispatches on the basename of argv[0] (busybox style) so the
// installed symlinks (srun, sbatch, scancel, ...) all resolve to this binary.
// Invoking "slurm-shim <subcommand>" behaves identically to the
// matching symlink, which is what the test suite and symlink-averse sites use.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	envcmd "github.com/hpc-gridware/slurm-shim/internal/cli/env"
	"github.com/hpc-gridware/slurm-shim/internal/cli/ports"
	"github.com/hpc-gridware/slurm-shim/internal/cli/sacct"
	"github.com/hpc-gridware/slurm-shim/internal/cli/sbatch"
	"github.com/hpc-gridware/slurm-shim/internal/cli/scancel"
	"github.com/hpc-gridware/slurm-shim/internal/cli/scontrol"
	"github.com/hpc-gridware/slurm-shim/internal/cli/sinfo"
	"github.com/hpc-gridware/slurm-shim/internal/cli/squeue"
	"github.com/hpc-gridware/slurm-shim/internal/cli/srun"
	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/stepper"
	"github.com/hpc-gridware/slurm-shim/internal/version"
)

// commands is the set of names this binary answers to. The value is the
// user-facing command name used in diagnostics. Symlink aliases
// and the equivalent "slurm-shim <name>" subcommand share one entry so both
// invocation forms take an identical code path. Every command is
// a stub in this build; real behavior lands in later milestones.
var commands = map[string]string{
	"srun":     "srun",
	"sbatch":   "sbatch",
	"sacct":    "sacct",
	"scancel":  "scancel",
	"scontrol": "scontrol",
	"squeue":   "squeue",
	"sinfo":    "sinfo",

	// Fabricator: symlink slurm-shim-env, subcommand form "slurm-shim env".
	"slurm-shim-env": "slurm-shim-env",
	"env":            "slurm-shim-env",

	// Per-host stepper: symlink slurm-shim-stepper, subcommand "stepper".
	"slurm-shim-stepper": "slurm-shim-stepper",
	"stepper":            "slurm-shim-stepper",

	// Internal per-rank trampoline; not user-invoked, dispatched by the stepper.
	"rank-exec": "rank-exec",

	// Site diagnostic: the TCP ranges that must be open between nodes. Subcommand
	// only -- there is no SLURM command of this name to shadow.
	"ports": "ports",
}

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:], os.Stdout, os.Stderr))
}

// run resolves the effective command from arg0 and returns the process exit
// code. Output streams are injected so tests can drive dispatch without exec;
// stdout carries command output only, stderr carries diagnostics.
func run(arg0 string, args []string, stdout, stderr io.Writer) int {
	name := arg0

	// When invoked as the real binary, the first positional token selects the
	// subcommand. A bare "slurm-shim --version" is handled before
	// the shift so it reports the top-level version.
	if name == "slurm-shim" {
		if hasVersionFlag(args) {
			fmt.Fprintln(stdout, version.String(version.DefaultCompat))
			return 0
		}
		if len(args) == 0 {
			fmt.Fprintln(stderr, "slurm-shim: error: no command given")
			return 1
		}
		name = args[0]
		args = args[1:]
	}

	cmd, known := commands[name]
	if !known {
		fmt.Fprintf(stderr, "slurm-shim: error: unknown command %q\n", name)
		return 1
	}

	// Every shimmed command answers --version/-V with the SLURM-parsable string
	// because frameworks feature-detect on it. Config-driven
	// compat version arrives with the config package; the default holds for now.
	if hasVersionFlag(args) {
		fmt.Fprintln(stdout, version.String(version.DefaultCompat))
		return 0
	}

	// Implemented commands dispatch to their package; the rest are stubs until
	// their milestone lands.
	switch cmd {
	case "slurm-shim-env":
		return envcmd.Run(args, stdout, stderr)
	case "srun":
		return srun.Run(args, stdout, stderr)
	case "sbatch":
		return sbatch.Run(args, stdout, stderr)
	case "sacct":
		return sacct.Run(args, stdout, stderr)
	case "scontrol":
		return scontrol.Run(args, stdout, stderr)
	case "scancel":
		return scancel.Run(args, stdout, stderr)
	case "squeue":
		return squeue.Run(args, stdout, stderr)
	case "sinfo":
		return sinfo.Run(args, stdout, stderr)
	case "slurm-shim-stepper":
		return stepper.Run(args, stderr)
	case "rank-exec":
		return stepper.RankExec(args)
	case "ports":
		// Site diagnostic, not a SLURM command: prints the TCP ranges that must be
		// open between nodes and the rules that open them.
		cfg, warnings, err := config.Load()
		if err != nil {
			fmt.Fprintf(stderr, "ports: error: %v\n", err)
			return 1
		}
		for _, w := range warnings {
			fmt.Fprintln(stderr, "ports: warning: "+w)
		}
		return ports.Run(cfg, stdout)
	}

	// Diagnostics go to stderr with the command-name prefix.
	fmt.Fprintf(stderr, "%s: error: not implemented in this build (%s)\n",
		cmd, version.String(version.DefaultCompat))
	return 1
}

// hasVersionFlag reports whether args request the version string. SLURM accepts
// both --version and -V across its client commands.
func hasVersionFlag(args []string) bool {
	for _, a := range args {
		if a == "--version" || a == "-V" {
			return true
		}
	}
	return false
}
