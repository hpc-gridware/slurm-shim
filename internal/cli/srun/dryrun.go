package srun

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
	"github.com/hpc-gridware/slurm-shim/internal/stepper"
)

// tokenPlaceholder stands in for the per-step control-channel token in the
// reported qrsh argv. A dry run never generates a token, and a real one must
// never be printed (REQ-CHN-003, SI-51).
const tokenPlaceholder = "<per-step token>"

// dryRun reports the step instead of launching it: where each rank would land,
// the qrsh command line that would carry its stepper, and the exact environment
// every rank would run with.
//
// It runs after placement, so everything reported is the real plan; it stops
// before the step id is consumed, the control channel is opened and any stepper is
// spawned, so a dry run leaves no trace in the job's state.
//
// The report goes to stderr: srun's stdout is the ranks' own output stream
// (mux.Demux writes there), so a caller capturing it must not receive a report.
// The exit code is the one the real step would return, so the mode works as a gate.
func (s *supervisor) dryRun() int {
	out := s.stderr
	fmt.Fprintln(out, dryrun.Banner("srun"))

	// Resolve the slave-host launcher through the same factory launch() uses, so
	// the report cannot claim a backend the real step would not pick -- including
	// the case where the configured launcher is rejected outright.
	slave, launchErr := launch.For(s.cfg, s.self, out)
	qrsh := false
	if launchErr == nil {
		_, qrsh = slave.(launch.QrshLauncher)
	}

	fmt.Fprintf(out, "\nstep:\n")
	kv(out, "step id", fmt.Sprint(s.stepID))
	kv(out, "tasks", fmt.Sprintf("%d across %d node(s)", len(s.plan.Ranks), len(s.plan.Nodes)))
	kv(out, "command", dryrun.Command(s.opt.command[0], s.opt.command[1:]))

	if launchErr != nil {
		kv(out, "launcher", fmt.Sprintf("ERROR: %v", launchErr))
		fmt.Fprintf(out, "\nNo launcher could be built, so no step would run; the real srun exits %d here.\n",
			exitLauncher)
		return exitLauncher
	}
	kv(out, "launcher", s.launcherSummary(qrsh))

	// The preflight srun would run before a remote launch is read-only (qconf), so
	// a dry run reports the same verdict without launching anything.
	if qrsh && s.hasSlaveNode() {
		pf := launch.Preflight(context.Background(), gedata.ExecRunner{}, s.lay.Job.PEName)
		for _, e := range pf.Errors {
			kv(out, "preflight", "ERROR: "+e)
		}
		if !pf.OK() {
			fmt.Fprintf(out, "\nThe parallel environment cannot host this step; the real srun exits %d here.\n",
				exitLauncher)
			return exitLauncher
		}
	}

	base := s.baseEnv()
	fmt.Fprintf(out, "\nwould launch:\n")
	for _, node := range s.plan.Nodes {
		// The master host always launches locally (REQ-RUN-012); slave hosts use the
		// configured backend.
		if node.LayoutIndex == 0 || !qrsh {
			fmt.Fprintf(out, "  %s: %s stepper --envelope <routing> (fork/exec, no qrsh)\n",
				node.Host, dryrun.Quote(s.self))
			continue
		}
		// Rendered by the launcher's own argv builder so it cannot drift from the
		// command line Grid Engine actually receives.
		fmt.Fprintf(out, "  %s: %s\n", node.Host, dryrun.Command("qrsh",
			launch.QrshPreview(s.self, node.Host, "<routing>", tokenPlaceholder)))
	}

	fmt.Fprintf(out, "\nranks:\n")
	for ni, node := range s.plan.Nodes {
		spec := s.stepSpec(base, ni)
		for _, r := range spec.Ranks {
			fmt.Fprintf(out, "  rank %d on %s: %s\n", r.Rank, node.Host, rankSummary(r))
		}
	}

	fmt.Fprintf(out, "\nrank environment (shared by every rank):\n")
	printEnv(out, stepEnv(base))
	for ni, node := range s.plan.Nodes {
		spec := s.stepSpec(base, ni)
		for _, r := range spec.Ranks {
			fmt.Fprintf(out, "\nrank %d (%s) adds:\n", r.Rank, node.Host)
			// The stepper's own overlay builder, so SLURMD_NODENAME and
			// CUDA_VISIBLE_DEVICES -- which it adds, not the planner -- are reported.
			printEnv(out, stepper.RankOverlay(r, node.Host))
		}
	}
	return 0
}

// stepEnv is the shared rank environment reduced to what the shim itself sets.
// The full base is the caller's whole environment (srun's default --export=ALL),
// which would bury the contract under the user's shell; the SLURM_* contract and
// the per-rank deltas are what a dry run is being asked about.
//
// The shim's own control namespace is excluded entirely. SLURM_SHIM_TOKEN is the
// sole authenticator of the control channel and must never be printed
// (REQ-CHN-003, SI-51); the rest (CONFIG, TASK_POLICY, DISABLE, DRY_RUN) are
// client inputs rather than part of the environment contract, and unsetPreamble
// says so.
func stepEnv(base []string) []string {
	var out []string
	for _, kv := range base {
		if strings.HasPrefix(kv, "SLURM_SHIM_") {
			continue
		}
		if strings.HasPrefix(kv, "SLURM_") || strings.HasPrefix(kv, "SLURMD_") ||
			strings.HasPrefix(kv, "MASTER_") || strings.HasPrefix(kv, "CUDA_VISIBLE_DEVICES=") {
			out = append(out, kv)
		}
	}
	sort.Strings(out)
	return out
}

// printEnv writes KEY=VALUE lines, escaping control characters so a value carried
// in from the environment cannot repaint the report on a terminal.
func printEnv(out io.Writer, kvs []string) {
	for _, e := range kvs {
		fmt.Fprintf(out, "  %s\n", dryrun.Escape(e))
	}
}

func kv(out io.Writer, key, value string) { fmt.Fprintf(out, "  %-19s %s\n", key, value) }

// rankSummary is the one-line placement of a rank: its cpuset, devices and where
// its output goes.
func rankSummary(r proto.RankSpec) string {
	parts := []string{fmt.Sprintf("local %d", r.Local)}
	if r.Cpuset != "" {
		parts = append(parts, "cpuset "+r.Cpuset)
	}
	if len(r.GPUs) > 0 {
		parts = append(parts, "gpus "+joinInts(r.GPUs))
	}
	switch {
	case r.StdoutFile != "" && r.StdoutFile == r.StderrFile:
		parts = append(parts, "output -> "+r.StdoutFile)
	case r.StdoutFile != "":
		parts = append(parts, "stdout -> "+r.StdoutFile)
		if r.StderrFile != "" {
			parts = append(parts, "stderr -> "+r.StderrFile)
		}
	default:
		parts = append(parts, "output streamed to srun")
	}
	return strings.Join(parts, ", ")
}

// launcherSummary names the backend each host would use, since the local launcher
// (dev/test) and qrsh tight integration behave very differently.
func (s *supervisor) launcherSummary(qrsh bool) string {
	if !qrsh {
		return fmt.Sprintf("local fork/exec on every host (launcher: %q -- no tight integration)", s.cfg.Launcher)
	}
	if !s.hasSlaveNode() {
		return "local on the master host (no slave host in this step, no qrsh needed)"
	}
	return "qrsh -inherit for slave hosts, local on the master host"
}

// hasSlaveNode reports whether any node of the step runs off the master, which is
// what makes the launch remote.
func (s *supervisor) hasSlaveNode() bool {
	for _, n := range s.plan.Nodes {
		if n.LayoutIndex != 0 {
			return true
		}
	}
	return false
}

func joinInts(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ",")
}
