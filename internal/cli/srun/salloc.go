package srun

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/slurmopts"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
	"github.com/hpc-gridware/slurm-shim/internal/version"
)

// RunSalloc is the salloc entry point. It creates an allocation and runs one
// command in it -- the user's shell when none is given -- on the allocation's
// first node, with a terminal when salloc runs on one. That is SLURM's
// behaviour under LaunchParameters=use_interactive_step; its default runs the
// shell on the submit host instead, which Grid Engine has no equivalent for.
// srun inside the shell launches steps across the whole allocation, and the
// allocation ends with the command, whose exit status salloc returns.
//
// salloc always creates a new allocation, also from inside one, as SLURM does.
func RunSalloc(args []string, stdout, stderr io.Writer) int {
	cfg, warns, err := config.Load()
	if err != nil {
		errln(stderr, "salloc: error: loading config: "+err.Error())
		return 1
	}
	for _, w := range warns {
		errln(stderr, "salloc: warning: "+w)
	}
	// SALLOC_* input variables go first, so a flag on the command line wins.
	args = append(submit.InputEnv(submit.SallocInputEnv, os.LookupEnv), args...)
	opt, err := parseFlagsWith(slurmopts.For("salloc"), args, cfg.StrictFlags, stderr)
	if err != nil {
		errln(stderr, strings.Replace(err.Error(), "srun: ", "salloc: ", 1))
		return 1
	}
	if opt.version {
		fmt.Fprintln(stdout, version.String(cfg.CompatVersion))
		return 0
	}
	if slices.Contains(opt.untranslated, "--no-shell") {
		errln(stderr, "salloc: error: --no-shell is not supported: a Grid Engine allocation lives only as long as its command")
		return 1
	}
	for _, w := range opt.warnings {
		errln(stderr, "salloc: warning: "+w)
	}
	if len(opt.command) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		opt.command = []string{shell}
	}

	spec, part, ok, code := buildSession(cfg, opt, "salloc", stderr)
	if !ok {
		return code
	}
	spec.Command = opt.command
	if stdinIsTerminal() && !backgroundOfTerminal() {
		spec.Pty = true
		return execTerminalSession("salloc", spec, part, opt.testOnly, stderr)
	}
	if dashCommand("salloc", spec.Command, stderr) {
		return 1
	}
	if isDryRun(opt) {
		return dryRunSession("salloc", "allocation", spec, part, stderr)
	}
	code, err = launch.RunSession(spec, opt.input == "none" || backgroundOfTerminal())
	if err != nil {
		errln(stderr, "salloc: error: "+err.Error())
		return exitLauncher
	}
	return code
}
