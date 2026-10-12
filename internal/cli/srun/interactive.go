package srun

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

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
	spec, part, ok, code := buildSession(cfg, opt, "srun", stderr)
	if !ok {
		return code
	}
	spec.Pty = true
	spec.Command = opt.command
	return execTerminalSession("srun", spec, part, opt.testOnly, stderr)
}

// execTerminalSession starts a session on a terminal: it reports it on a dry
// run, else replaces the process with qrsh.
func execTerminalSession(prog string, spec launch.SessionSpec, part config.Partition, testOnly bool, stderr io.Writer) int {
	if dashCommand(prog, spec.Command, stderr) {
		return 1
	}
	if dryrun.Enabled() || testOnly {
		return dryRunSession(prog, "interactive session", spec, part, stderr)
	}

	warnIfNoPtyDaemon(gedata.ExecRunner{}, stderr)

	// One honest line before the process is replaced: srun cannot print SLURM's
	// "queued and waiting for resources" at the right moment once it has exec'd,
	// and the wait is otherwise silent (verified).
	fmt.Fprintf(stderr, "%s: requesting an interactive allocation (qrsh -now no); press Ctrl-C to cancel while it waits\n", prog)

	if err := launch.ExecSession(spec); err != nil {
		errln(stderr, prog+": error: "+err.Error())
		return exitLauncher
	}
	return 0 // unreachable: ExecSession replaces the process on success
}

// dashCommand refuses a session command beginning with "-": qrsh would read
// it as its own option, since Grid Engine has no "--" terminator.
func dashCommand(prog string, command []string, stderr io.Writer) bool {
	if len(command) > 0 && strings.HasPrefix(command[0], "-") {
		errln(stderr, fmt.Sprintf("%s: error: command %q begins with '-', which qrsh would read as an option", prog, command[0]))
		return true
	}
	return false
}

// isDryRun reports whether the command only reports what it would run.
func isDryRun(opt *options) bool { return dryrun.Enabled() || opt.testOnly }

// runStandalone runs `srun <flags> <command>` outside an allocation, without
// --pty, as SLURM does: it creates an allocation, runs the command there as one
// step -- every task, on every node -- and returns when the step ends, with its
// exit status. The allocation is a qrsh session whose command is this shim's
// own srun with the step options, so inside it the step is launched exactly as
// in a batch job. Output streams back through qrsh. The step's tasks get no
// stdin -- the step launcher does not forward it, in a batch job either -- so
// qrsh gets /dev/null rather than consume input the tasks would never see
// (a `while read` loop calling srun keeps its lines).
func runStandalone(cfg *config.Config, opt *options, stderr io.Writer) int {
	spec, part, ok, code := buildSession(cfg, opt, "srun", stderr)
	if !ok {
		return code
	}
	self, err := os.Executable()
	if err != nil {
		errln(stderr, "srun: error: locating the shim binary: "+err.Error())
		return exitLauncher
	}
	spec.Command = append(append([]string{self, "srun"}, stepArgs(opt)...), "--")
	spec.Command = append(spec.Command, opt.command...)
	if opt.input != "" && opt.input != "none" {
		errln(stderr, "srun: warning: --input="+opt.input+" is not translated; the tasks get no stdin")
	}
	if isDryRun(opt) {
		return dryRunSession("srun", "allocation and step", spec, part, stderr)
	}
	code, err = launch.RunSession(spec, true)
	if err != nil {
		errln(stderr, "srun: error: "+err.Error())
		return exitLauncher
	}
	return code
}

// stepArgs are the srun options that shape the step, passed on to the srun
// that runs inside the new allocation. The allocation options (partition,
// time, memory, GPUs, account, job name) already shaped the allocation.
func stepArgs(opt *options) []string {
	var a []string
	num := func(flag string, n int) {
		if n > 0 {
			a = append(a, flag+"="+strconv.Itoa(n))
		}
	}
	num("--ntasks", opt.req.NTasks)
	num("--nodes", opt.req.Nodes)
	num("--ntasks-per-node", opt.req.TasksPerNode)
	num("--cpus-per-task", opt.req.CPUsPerTask)
	num("--gpus-per-task", opt.req.GPUsPerTask)
	str := func(flag, v string) {
		if v != "" {
			a = append(a, flag+"="+v)
		}
	}
	str("--output", opt.output)
	str("--error", opt.errorPat)
	str("--kill-on-bad-exit", opt.killFlag)
	str("--gpu-bind", opt.gpuBind)
	str("--x-elastic", opt.elastic)
	str("--export", opt.exportSpec)
	if opt.label {
		a = append(a, "--label")
	}
	return a
}

// buildSession translates the allocation options -- partition, geometry,
// time, memory, GPUs, account, job name, working directory, export -- into a
// qrsh session. prog names the command in messages. It leaves Pty and Command
// to the caller.
func buildSession(cfg *config.Config, opt *options, prog string, stderr io.Writer) (launch.SessionSpec, config.Partition, bool, int) {
	part, ok, code := resolvePartition(cfg, opt, prog, stderr)
	if !ok {
		return launch.SessionSpec{}, part, false, code
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
		errln(stderr, prog+": error: "+err.Error())
		return launch.SessionSpec{}, part, false, 1
	}

	// Pin the layout (-par / -w e) when -N / --ntasks-per-node ask for one and the
	// cluster supports it -- the same resolver and OCS-version probe sbatch uses.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.QstatTimeout.Duration)
	defer cancel()
	rule, warns := submit.AllocationRuleFor(ctx, gedata.ExecRunner{}, cfg, req, part, slots)
	if w := submit.MemoryComplexWarning(cfg, req); w != "" {
		warns = append(warns, w)
	}
	if w := interactiveDeviceMaskWarning(cfg, req.HaveGPUs); w != "" {
		warns = append(warns, w)
	}
	if len(opt.req.Nodelist) > 0 {
		warns = append(warns, "--nodelist is not applied to a new allocation; Grid Engine chooses its hosts")
	}
	if opt.immediate != "" && opt.immediate != "set" {
		warns = append(warns, "--immediate="+opt.immediate+": Grid Engine starts the job at once or refuses it; "+
			"the wait of "+opt.immediate+" seconds is not applied")
	}
	for _, w := range warns {
		errln(stderr, prog+": warning: "+w)
	}

	return launch.SessionSpec{
		Immediate:      opt.immediate != "",
		Queue:          part.Queue,
		PE:             part.PE,
		Slots:          slots,
		AllocationRule: rule.Value,
		VerifyGeometry: submit.VerifyGeometry(cfg, req),
		Resources:      joinRequests(part.Request, submit.ResourceList(cfg, req)),
		Account:        opt.account,
		JobName:        opt.jobName,
		Chdir:          opt.chdir,
		Export:         submit.ExportArgs(opt.exportSpec),
	}, part, true, 0
}

// resolvePartition applies SLURM's DEFAULT-partition behavior and looks the
// partition up in config, returning the resolved partition or a non-zero code.
func resolvePartition(cfg *config.Config, opt *options, prog string, stderr io.Writer) (config.Partition, bool, int) {
	if opt.partition == "" {
		opt.partition = cfg.DefaultPartition
	}
	if opt.partition == "" {
		errln(stderr, prog+": error: no partition specified (and no default_partition configured)")
		return config.Partition{}, false, 1
	}
	part, ok := cfg.Partitions[opt.partition]
	if !ok {
		errln(stderr, fmt.Sprintf("%s: error: unknown partition %q", prog, opt.partition))
		return config.Partition{}, false, 1
	}
	return part, true, 0
}

// dryRunSession reports the qrsh invocation without creating a job or
// touching the terminal, rendering the exact argv SessionArgs builds so the
// report cannot drift from what a real run submits. Secret -v values are
// redacted.
func dryRunSession(prog, what string, spec launch.SessionSpec, part config.Partition, stderr io.Writer) int {
	out := stderr
	fmt.Fprintln(out, dryrun.Banner(prog))
	fmt.Fprintf(out, "\n%s:\n", what)
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

// warnIfNoPtyDaemon warns when the queue's interactive daemon is not the builtin
// IJS: verified that with an external rsh_daemon (e.g. "sshd -i") the fabricated
// SLURM_* environment still arrives but -pty y allocates no terminal, so the
// session lands without a tty. Read-only and best-effort: any probe failure is
// silent, since it must never block a session that would otherwise work.
func warnIfNoPtyDaemon(r gedata.Runner, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _, exit, err := r.Run(ctx, "qconf", "-sconf")
	if err != nil || exit != 0 {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "rsh_daemon" && !strings.EqualFold(f[1], "builtin") {
			fmt.Fprintf(stderr, "srun: warning: this cluster's rsh_daemon is %q, not builtin; "+
				"the session's SLURM_* environment will be set but it may have no terminal "+
				"(add -t to rsh_command, or use a builtin-IJS queue)\n", f[1])
			return
		}
	}
}

// joinRequests joins -l lists, skipping empty ones.
func joinRequests(lists ...string) string {
	var out []string
	for _, l := range lists {
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, ",")
}
