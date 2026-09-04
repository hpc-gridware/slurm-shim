package srun

import (
	"fmt"
	"io"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

// runInteractive turns `srun --pty <flags> <command>` on a login node into an
// interactive Grid Engine session: it translates the SLURM flags to a qrsh
// invocation and, unless this is a dry run, replaces the shim process with qrsh
// (which owns the terminal, the allocation wait, signals and the exit status).
//
// It is reached only when srun is outside any allocation and --pty was given;
// inside an allocation --pty stays a step (handled in Run).
func runInteractive(cfg *config.Config, opt *options, stderr io.Writer) int {
	part, ok, code := resolvePartition(cfg, opt, stderr)
	if !ok {
		return code
	}

	req := submit.Request{
		Partition:     opt.partition,
		Nodes:         opt.req.Nodes,
		NTasks:        opt.req.NTasks,
		NTasksPerNode: opt.req.TasksPerNode,
		CPUsPerTask:   opt.req.CPUsPerTask,
		HaveNTasks:    opt.req.NTasks > 0,
		HavePerNode:   opt.req.TasksPerNode > 0,
		HaveTime:      opt.haveTime,
		TimeSec:       opt.timeSec,
		Mem:           opt.mem,
		HaveGPUs:      opt.haveGPUs,
		GPUs:          opt.gpus,
		ExportSpec:    opt.exportSpec,
	}

	slots, err := submit.Slots(req, part)
	if err != nil {
		errln(stderr, "srun: error: "+err.Error())
		return 1
	}

	spec := launch.SessionSpec{
		Queue:     part.Queue,
		PE:        part.PE,
		Slots:     slots,
		Resources: submit.ResourceList(cfg, req),
		Account:   opt.account,
		JobName:   opt.jobName,
		Chdir:     opt.chdir,
		Export:    submit.ExportArgs(opt.exportSpec),
		Command:   opt.command,
	}

	// A command beginning with "-" would be read by qrsh as its own option, since
	// Grid Engine has no "--" terminator. Refuse rather than mis-submit.
	if len(spec.Command) > 0 && len(spec.Command[0]) > 0 && spec.Command[0][0] == '-' {
		errln(stderr, fmt.Sprintf("srun: error: interactive command %q begins with '-', which qrsh would read as an option", spec.Command[0]))
		return 1
	}

	if dryrun.Enabled() || opt.testOnly {
		return dryRunInteractive(spec, part, stderr)
	}

	// One honest line before the process is replaced: srun cannot print SLURM's
	// "queued and waiting for resources" at the right moment once it has exec'd,
	// and the wait is otherwise silent (verified).
	fmt.Fprintln(stderr, "srun: requesting an interactive allocation (qrsh -now no); press Ctrl-C to cancel while it waits")

	if err := launch.ExecSession(spec); err != nil {
		errln(stderr, "srun: error: "+err.Error())
		return exitLauncher
	}
	return 0 // unreachable: ExecSession replaces the process on success
}

// resolvePartition applies SLURM's DEFAULT-partition behavior and looks the
// partition up in config, returning the resolved partition or a non-zero code.
func resolvePartition(cfg *config.Config, opt *options, stderr io.Writer) (config.Partition, bool, int) {
	if opt.partition == "" {
		opt.partition = cfg.DefaultPartition
	}
	if opt.partition == "" {
		errln(stderr, "srun: error: no partition specified (and no default_partition configured)")
		return config.Partition{}, false, 1
	}
	part, ok := cfg.Partitions[opt.partition]
	if !ok {
		errln(stderr, fmt.Sprintf("srun: error: unknown partition %q", opt.partition))
		return config.Partition{}, false, 1
	}
	return part, true, 0
}

// dryRunInteractive reports the qrsh invocation without creating a job or
// touching the terminal, rendering the exact argv SessionArgs builds so the
// report cannot drift from what a real run submits. Secret -v values are
// redacted.
func dryRunInteractive(spec launch.SessionSpec, part config.Partition, stderr io.Writer) int {
	out := stderr
	fmt.Fprintln(out, dryrun.Banner("srun"))
	fmt.Fprintf(out, "\ninteractive session:\n")
	kv(out, "partition", fmt.Sprintf("queue %s, pe %s, %d slot(s)", part.Queue, part.PE, spec.Slots))
	argv := redactSessionArgs(launch.SessionArgs(spec))
	kv(out, "would run", dryrun.Command(gedata.ResolveCommand("qrsh"), argv))
	return 0
}

// redactSessionArgs masks the value of every -v assignment for display, keeping
// -V and every other flag intact (secrets must not reach terminal scrollback).
func redactSessionArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-v" {
			out[i+1] = dryrun.RedactAssignment(out[i+1])
		}
	}
	return out
}
