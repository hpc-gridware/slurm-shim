/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

package srun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/cpubind"
	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

// srun --mpi=pmix hands the step to OpenMPI's own launcher instead of
// implementing PMIx: srun plans the step as usual, puts the step's geometry
// into the SLURM_* variables OpenMPI's SLURM allocator reads, and execs
// mpirun. mpirun starts its daemons through srun again (an external-launcher
// step, extlauncher.go); the daemons run the PMIx server and the ranks. The
// ranks then belong to mpirun, so srun's own output labels, per-rank files,
// exit-code rule and spares do not apply.

// normalizeMPI maps an --mpi or SLURM_MPI_TYPE value to the type srun runs.
// "" means not given. SLURM's versioned pmix names select the same path:
// OpenMPI's mpirun brings its own PMIx.
func normalizeMPI(v, source string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return "", nil
	case config.MPINone:
		return config.MPINone, nil
	case "pmix", "pmix_v3", "pmix_v4", "pmix_v5":
		return config.MPIPMIx, nil
	}
	return "", fmt.Errorf("srun: error: %s=%s is not supported; slurm-shim runs --mpi=none and --mpi=pmix (OpenMPI's mpirun)", source, v)
}

// resolveMPI picks the MPI type the caller asked for: --mpi, else
// SLURM_MPI_TYPE; "" when neither is set. The site's mpi.default applies
// later, and only to a step inside an allocation (applyMPIDefault).
func resolveMPI(flag, env string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return normalizeMPI(env, "SLURM_MPI_TYPE")
}

// applyMPIDefault applies mpi.default (SLURM's MpiDefault) to a step that
// named no MPI type. A --pty step keeps running as one: MpiDefault adds MPI
// support under SLURM, it never changes how an interactive step runs.
func applyMPIDefault(opt *options, cfg *config.Config) {
	if opt.mpi == "" && !opt.pty && cfg.MPI.Default == config.MPIPMIx {
		opt.mpi = config.MPIPMIx
	}
}

// mpiLauncher is the mpirun srun execs.
type mpiLauncher struct {
	path   string
	banner string // the --version line that identified it
	prrte  bool   // PRRTE-based: OpenMPI 5 or prterun; else OpenMPI 4.1 (ORTE)
}

var (
	ompiVersion  = regexp.MustCompile(`\(Open MPI\) ([0-9]+)\.([0-9]+)[0-9.a-z]*`)
	prrteVersion = regexp.MustCompile(`\(PRRTE\) [0-9]`)
)

// parseLauncher identifies a launcher from its --version output. Only Open MPI
// 4.1 or newer, and PRRTE's prterun, use the SLURM launcher path this needs;
// another MPI's mpirun (Intel MPI, MPICH) first on PATH is the likely mistake,
// so its own words are quoted back.
func parseLauncher(path, out string) (*mpiLauncher, error) {
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	if m := ompiVersion.FindStringSubmatch(out); m != nil {
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		v := strings.TrimPrefix(m[0], "(Open MPI) ")
		if major < 4 || (major == 4 && minor < 1) {
			return nil, fmt.Errorf("srun: error: --mpi=pmix needs Open MPI 4.1 or newer; %s is Open MPI %s", path, v)
		}
		return &mpiLauncher{path: path, banner: first, prrte: major >= 5}, nil
	}
	if prrteVersion.MatchString(out) {
		return &mpiLauncher{path: path, banner: first, prrte: true}, nil
	}
	if first == "" {
		first = "no version output"
	}
	return nil, fmt.Errorf("srun: error: --mpi=pmix needs Open MPI's mpirun; %s says %q (set mpi.launcher in config.yaml)", path, first)
}

// findLauncher resolves mpi.launcher (default: mpirun on PATH) and asks it
// what it is.
func findLauncher(cfg *config.Config) (*mpiLauncher, error) {
	name := "mpirun"
	if cfg.MPI.Launcher != "" {
		name = cfg.MPI.Launcher
	}
	path, out, err := launch.ProbeVersion(name)
	if errors.Is(err, launch.ErrCommandNotFound) {
		return nil, fmt.Errorf("srun: error: --mpi=pmix: %s not found (put Open MPI's bin directory on PATH, or set mpi.launcher)", name)
	}
	if err != nil {
		return nil, fmt.Errorf("srun: error: --mpi=pmix: %v", err)
	}
	return parseLauncher(path, out)
}

// launcherArgv is the mpirun command line: one rank per planned task, mapped
// by slot (OpenMPI's default fills each node's slots in turn, which is SLURM's
// block distribution), each rank started through rank-env with the step's
// variables, so a rank names this step and not the daemon step that forked it.
func launcherArgv(l *mpiLauncher, p *plan.StepPlan, label bool, self string, vars, command []string) []string {
	argv := []string{l.path, "-n", strconv.Itoa(p.NTasks)}
	if p.CPUsPerTask > 1 {
		pe := "PE="
		if l.prrte {
			pe = "pe="
		}
		argv = append(argv, "--map-by", "slot:"+pe+strconv.Itoa(p.CPUsPerTask))
	}
	if label {
		if l.prrte {
			argv = append(argv, "--output", "tag")
		} else {
			argv = append(argv, "--tag-output")
		}
	}
	argv = append(argv, self, "rank-env")
	argv = append(argv, vars...)
	argv = append(argv, "--")
	return append(argv, command...)
}

// mpiNodesEnv names the pmix step's nodes for the srun mpirun starts its
// daemons with. When the step leaves out the host mpirun runs on, OpenMPI
// needs a daemon on every step node and omits the node list from that srun
// line; the daemon step reads the nodes from here instead.
const mpiNodesEnv = "SLURM_SHIM_MPI_NODELIST"

// stepVars are the step's SLURM_* values: the stepper's step variables plus
// SLURM_NODELIST, which with SLURM_TASKS_PER_NODE is what OpenMPI's SLURM
// allocator reads, so a step on part of the allocation maps onto that part
// only (srun itself reads the allocation from layout.json).
func stepVars(lay *layout.Layout, p *plan.StepPlan, stepID int) []string {
	// stepShadows reads only lay, plan and stepID.
	s := &supervisor{lay: lay, plan: p, stepID: stepID}
	return append(s.stepShadows(), "SLURM_NODELIST="+encoders.CompressNodelist(joinHosts(p.Nodes)))
}

// launcherEnv is the environment mpirun starts with: the caller's, with the
// step's variables over the job's. The *_MCA_ras variables select OpenMPI's
// SLURM allocator: PRRTE's Grid Engine one would win in a shim job and read
// the whole job's PE_HOSTFILE.
func launcherEnv(base, vars []string, p *plan.StepPlan) []string {
	overlay := append(append([]string{}, vars...),
		mpiNodesEnv+"="+encoders.CompressNodelist(joinHosts(p.Nodes)),
		"PRTE_MCA_ras=slurm",
		"OMPI_MCA_ras=slurm",
	)
	return dedupEnv(base, overlay)
}

// pmixRefusal names the srun options delegation cannot honour, or "" when the
// step can run. Warnings cover what runs with a different meaning.
func pmixRefusal(opt *options) (refusal string, warnings []string) {
	switch {
	case opt.pty:
		return "--pty cannot run through mpirun", nil
	case opt.elastic == config.ElasticOn:
		return "--x-elastic=on: an MPI job does not survive losing a rank", nil
	case strings.Contains(opt.output, "%") || strings.Contains(opt.errorPat, "%"):
		return "per-rank output patterns (-o/-e with %) cannot apply: the ranks write through mpirun", nil
	}
	if opt.req.GPUsPerTask > 0 || opt.gpuBind != "" {
		warnings = append(warnings, "--gpus-per-task/--gpu-bind are not applied under --mpi=pmix; every rank sees the node's GPUs")
	}
	if opt.exportSpec != "" && opt.exportSpec != "ALL" {
		warnings = append(warnings, "--export is not applied under --mpi=pmix; the ranks get mpirun's environment")
	}
	return "", warnings
}

// runPMIx execs mpirun for the planned step, or reports it under a dry run.
// The dry run still asks the launcher for its version: whether it is Open
// MPI, and which, decides the command line it reports.
func runPMIx(cfg *config.Config, opt *options, lay *layout.Layout, p *plan.StepPlan, stepID int, dry bool, stderr io.Writer) int {
	refusal, warnings := pmixRefusal(opt)
	if refusal != "" {
		errln(stderr, "srun: error: "+refusal)
		return 1
	}
	for _, w := range warnings {
		errln(stderr, "srun: warning: "+w)
	}
	l, err := findLauncher(cfg)
	if err != nil {
		errln(stderr, err.Error())
		return 1
	}
	self, err := os.Executable()
	if err != nil {
		errln(stderr, "srun: error: locating self: "+err.Error())
		return 1
	}
	vars := stepVars(lay, p, stepID)
	argv := launcherArgv(l, p, opt.label, self, vars, opt.command)
	// Without CPU facts (non-Linux, unreadable mask) mpirun binds as before.
	master, err := cpubind.ThisNode()
	if err != nil {
		errln(stderr, "srun: warning: cannot read the CPUs this job may use here; OpenMPI binds as it chooses: "+err.Error())
	}
	holdings := make([]cpubind.Holding, len(p.Nodes))
	for i, n := range p.Nodes {
		holdings[i] = cpubind.Holding{Slots: n.Slots, HostSlots: n.HostSlots}
	}
	launcherVars := append(append([]string{}, vars...),
		cpubind.OpenMPIOverride(opt.bindMode, master, holdings, os.Getenv)...)
	if dry {
		reportPMIx(stderr, l, p, stepID, argv, launcherEnv(nil, launcherVars, p))
		return 0
	}
	if opt.chdir != "" {
		if err := os.Chdir(opt.chdir); err != nil {
			errln(stderr, "srun: error: --chdir: "+err.Error())
			return 1
		}
	}
	if err := redirectOutputs(opt.output, opt.errorPat); err != nil {
		errln(stderr, "srun: error: "+err.Error())
		return 1
	}
	err = launch.Exec(l.path, argv, launcherEnv(os.Environ(), launcherVars, p))
	errln(stderr, fmt.Sprintf("srun: error: running %s: %v", l.path, err))
	return 1
}

// redirectOutputs points stdout at -o and stderr at -e (each truncated), so a
// plain file name still collects mpirun's output. One file named by both is
// opened once: two private offsets would overwrite each other's lines.
func redirectOutputs(output, errorPat string) error {
	if output != "" {
		if err := redirect(output, 1); err != nil {
			return err
		}
		if errorPat == output {
			return unix.Dup2(1, 2)
		}
	}
	if errorPat != "" {
		return redirect(errorPat, 2)
	}
	return nil
}

// redirect points fd at path, truncated.
func redirect(path string, fd int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return unix.Dup2(int(f.Fd()), fd)
}

// reportPMIx is the dry-run report: what srun would exec, with which step
// variables, and nothing launched.
func reportPMIx(out io.Writer, l *mpiLauncher, p *plan.StepPlan, stepID int, argv, overrides []string) {
	fmt.Fprintln(out, dryrun.Banner("srun"))
	fmt.Fprintf(out, "\nstep:\n")
	kv(out, "step id", fmt.Sprint(stepID))
	kv(out, "tasks", fmt.Sprintf("%d across %d node(s)", p.NTasks, len(p.Nodes)))
	kv(out, "launcher", fmt.Sprintf("%s (%s) -- --mpi=pmix: mpirun runs the ranks", l.path, l.banner))
	fmt.Fprintf(out, "\nwould exec:\n")
	fmt.Fprintf(out, "  %s\n", dryrun.Command(argv[0], argv[1:]))
	fmt.Fprintf(out, "\nlauncher environment (over the caller's):\n")
	printEnv(out, overrides)
}

// rankEnvDrop are inherited variables rank-env removes: SLURM_NODEID is
// recomputed (and stays unset when the host is not in the step), SLURM_GTIDS
// would name the daemon step's tasks, and mpiNodesEnv is for the daemon srun.
var rankEnvDrop = []string{"SLURM_NODEID", "SLURM_GTIDS", mpiNodesEnv}

// RankEnv is the `slurm-shim rank-env [VAR=value...] -- command...`
// trampoline mpirun starts each rank through under srun --mpi=pmix. The
// assignments are the step's variables: a rank on another node is forked by a
// daemon and inherits the daemon step's. It then sets SLURM_PROCID,
// SLURM_LOCALID and SLURM_NODEID from what OpenMPI tells the rank, and execs
// the command.
func RankEnv(args []string, stderr io.Writer) int {
	var vars []string
	for len(args) > 0 && args[0] != "--" {
		if !strings.Contains(args[0], "=") {
			errln(stderr, fmt.Sprintf("srun: error: rank-env: %q is not VAR=value; usage: rank-env [VAR=value...] -- command", args[0]))
			return 1
		}
		vars = append(vars, args[0])
		args = args[1:]
	}
	if len(args) > 0 {
		args = args[1:]
	}
	if len(args) == 0 {
		errln(stderr, "srun: error: rank-env: no command given")
		return 1
	}
	env := dedupEnv(dropEnv(os.Environ(), rankEnvDrop...), vars)
	env = dedupEnv(env, rankOverlay(envLookup(env), os.Hostname))
	err := launch.Exec(args[0], args, env)
	errln(stderr, fmt.Sprintf("srun: error: %v", err))
	if errors.Is(err, launch.ErrCommandNotFound) {
		return 127
	}
	return 126
}

// envLookup returns a getenv over env (last entry wins, as in dedupEnv).
func envLookup(env []string) func(string) string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return func(k string) string { return m[k] }
}

// rankOverlay maps OpenMPI's per-rank variables to SLURM's. The node index is
// the host's place in SLURM_STEP_NODELIST; a host not in it gets none.
func rankOverlay(getenv func(string) string, hostname func() (string, error)) []string {
	var out []string
	rank := getenv("PMIX_RANK")
	if rank == "" {
		rank = getenv("OMPI_COMM_WORLD_RANK")
	}
	if rank != "" {
		out = append(out, "SLURM_PROCID="+rank)
	}
	if local := getenv("OMPI_COMM_WORLD_LOCAL_RANK"); local != "" {
		out = append(out, "SLURM_LOCALID="+local)
	}
	host := getenv("SLURMD_NODENAME")
	if host == "" {
		host, _ = hostname()
	}
	if nodes, err := encoders.ExpandNodelist(getenv("SLURM_STEP_NODELIST")); err == nil && host != "" {
		for i, n := range nodes {
			if n == host || strings.SplitN(n, ".", 2)[0] == strings.SplitN(host, ".", 2)[0] {
				out = append(out, "SLURM_NODEID="+strconv.Itoa(i))
				break
			}
		}
	}
	return out
}
