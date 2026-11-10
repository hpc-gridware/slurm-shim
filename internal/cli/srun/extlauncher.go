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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/cpubind"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

// OpenMPI's mpirun runs inside a shim job as it does under SLURM: it sees
// SLURM_JOB_ID, selects its SLURM launcher (OpenMPI 4.1 ORTE, PRRTE in 5.x)
// and starts one daemon per node with srun, e.g. PRRTE 3.0:
//
//	srun --external-launcher --ntasks-per-node=1 --kill-on-bad-exit
//	     --mpi=none --cpu-bind=none --nodes=2 --nodelist=a,b --ntasks=2 prted ...
//
// The daemons run the PMIx server and fork the ranks, so the step must not
// pin them (every rank inherits the daemon's mask), must not replace one with
// a spare, and must put them on the nodes the launcher asked for. The helpers
// here implement the SLURM options that path needs.

// maxNodelistFile caps a --nodelist file: one name per node, so a megabyte is
// far beyond any allocation and still bounds what a stray path can make srun
// read.
const maxNodelistFile = 1 << 20

// expandNodelist expands --nodelist. A value containing "/" names a file of
// host names, one per line or comma-separated, as SLURM reads it (PRRTE
// passes its node list that way, since one argument hits the kernel's
// per-argument limit at scale). Order is kept.
func expandNodelist(value string) ([]string, error) {
	if !strings.Contains(value, "/") {
		return submit.ExpandHosts("--nodelist", value)
	}
	// O_NONBLOCK: a FIFO without a writer must not hang srun before the
	// regular-file check below can refuse it.
	f, err := os.OpenFile(value, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("--nodelist=%s: %w", value, err)
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("--nodelist=%s: not a regular file", value)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxNodelistFile+1))
	if err != nil {
		return nil, fmt.Errorf("--nodelist=%s: %w", value, err)
	}
	if len(data) > maxNodelistFile {
		return nil, fmt.Errorf("--nodelist=%s: file larger than %d bytes", value, maxNodelistFile)
	}
	var names []string
	for _, field := range strings.FieldsFunc(string(data), func(r rune) bool {
		return r == '\n' || r == '\r' || r == ','
	}) {
		if name := strings.TrimSpace(field); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("--nodelist=%s: no host names in the file", value)
	}
	hosts, err := submit.ExpandHosts("--nodelist", strings.Join(names, ","))
	if err != nil {
		return nil, fmt.Errorf("%w (from %s)", err, value)
	}
	return hosts, nil
}

// applyDistribution handles --distribution. Only "arbitrary" has an effect:
// task i runs on the i-th --nodelist entry, which PRRTE relies on to number
// its daemons. SLURM takes the node count from the list and refuses --nodes
// with it; the shim runs one task per entry and no node twice. Every other
// distribution stays untranslated: warned, or refused under strict_flags, as
// before the option was defined here.
func applyDistribution(opt *options, dist string, strict bool) error {
	switch dist {
	case "":
		return nil
	case "arbitrary":
		opt.distribution = dist
		if opt.req.Nodes > 0 {
			return fmt.Errorf("srun: error: --distribution=arbitrary takes the node count from --nodelist; do not combine it with --nodes")
		}
		if len(opt.req.Nodelist) == 0 {
			return fmt.Errorf("srun: error: --distribution=arbitrary needs --nodelist")
		}
		seen := map[string]bool{}
		for _, h := range opt.req.Nodelist {
			if seen[h] {
				return fmt.Errorf("srun: error: --distribution=arbitrary: node %q is listed twice; the shim runs at most one list entry per node", h)
			}
			seen[h] = true
		}
		if opt.req.TasksPerNode > 1 {
			return fmt.Errorf("srun: error: --distribution=arbitrary runs one task per --nodelist entry; --ntasks-per-node=%d conflicts", opt.req.TasksPerNode)
		}
		if opt.req.NTasks > 0 && opt.req.NTasks != len(opt.req.Nodelist) {
			return fmt.Errorf("srun: error: --distribution=arbitrary runs one task per --nodelist entry: --ntasks=%d, %d entries", opt.req.NTasks, len(opt.req.Nodelist))
		}
		opt.req.TasksPerNode = 1
		opt.req.Ordered = true
		return nil
	}
	if strict {
		return fmt.Errorf("srun: error: option --distribution is not supported by slurm-shim")
	}
	opt.warnings = append(opt.warnings, "option --distribution ignored (slurm-shim)")
	return nil
}

// applyCPUBind folds --cpu-bind, else SLURM_CPU_BIND (how OpenMPI 4.1 asks
// for it), into the step's binding mode (cpubind.ModeFor): "none" never pins,
// any type pins within the job's CPUs even on a shared node, nothing given
// lets each node decide (cpubind.ShouldPin). A map_cpu/mask_cpu list names
// absolute CPUs another job may hold, so it is not applied.
func applyCPUBind(opt *options, envBind string) {
	spec := opt.cpuBind
	if spec == "" {
		spec = envBind
	}
	opt.bindMode = cpubind.ModeFor(spec)
	if strings.Contains(spec, "map_cpu") || strings.Contains(spec, "mask_cpu") {
		opt.warnings = append(opt.warnings,
			"--cpu-bind="+spec+": the CPU list is not applied; tasks are pinned within the job's CPUs (slurm-shim)")
	}
}

// applyExternalLauncher shapes a daemon step (--external-launcher, or ORTE's
// orted line), whose tasks are
// an MPI launcher's daemons: never pinned (every rank would inherit one CPU),
// never handed to mpirun again (that would start mpirun from mpirun), and,
// without a node list, kept off the host srun runs on (the job's master),
// which the launcher serves itself. PRRTE 3.0 omits the list when its Grid
// Engine allocator is active.
func applyExternalLauncher(opt *options) {
	if !opt.launchesDaemons() {
		return
	}
	opt.bindMode = cpubind.ModeNone
	opt.mpi = config.MPINone
	if len(opt.req.Nodelist) == 0 && opt.req.Nodes == 0 {
		opt.req.AvoidMaster = true
	}
}

// isORTEDaemonStep reports whether the step starts the daemons of OpenMPI
// 4.1's mpirun. ORTE runs
//
//	srun --ntasks-per-node=1 --kill-on-bad-exit --mpi=none
//	     [--nodes=2 --nodelist=a,b] --ntasks=2 orted ...
//
// and, unlike PRRTE, does not say --external-launcher. An OpenMPI built with
// both Grid Engine and SLURM support (--with-sge --with-slurm) selects its
// Grid Engine allocator in a shim job, which leaves the node list out; the
// step must then stay off the master as an --external-launcher one does, or
// a daemon lands next to mpirun and the last node gets none.
//
// The match is ORTE's whole line shape, not the name alone: --mpi=none, one
// task per node and a command named orted. A site's own launch agent
// (orte_launch_agent) is not recognized; OMPI_MCA_ras=slurm makes ORTE pass
// the node list again.
func isORTEDaemonStep(opt *options) bool {
	return opt.mpi == config.MPINone && opt.req.TasksPerNode == 1 &&
		len(opt.command) > 0 && filepath.Base(opt.command[0]) == "orted"
}

// launchesDaemons reports whether the step starts an MPI launcher's daemons:
// --external-launcher (PRRTE), or ORTE's orted line, which does not say so.
func (opt *options) launchesDaemons() bool {
	return opt.externalLauncher || isORTEDaemonStep(opt)
}

// applyMPINodes places the daemon step of an srun --mpi=pmix step that left
// out the master. OpenMPI then needs a daemon on every step node and omits
// the node list (both ORTE and PRRTE, and ORTE does not say
// --external-launcher), so the nodes come from what runPMIx exported: the
// step's nodes in the allocation, without the host srun runs on.
func applyMPINodes(opt *options, lay *layout.Layout, nodes string) {
	if nodes == "" || len(opt.req.Nodelist) > 0 || opt.req.Nodes > 0 || len(lay.Nodes) == 0 {
		return
	}
	hosts, err := encoders.ExpandNodelist(nodes)
	if err != nil {
		return
	}
	inJob := map[string]bool{}
	for _, n := range lay.Nodes[1:] {
		inJob[n.Host] = true
	}
	var list []string
	for _, h := range hosts {
		if inJob[h] {
			list = append(list, h)
		}
	}
	if len(list) > 0 {
		opt.req.Nodelist = list
		opt.req.AvoidMaster = false
	}
}

// checkJobID accepts --jobid only for this job (own; -1 outside any): the
// shim cannot start a step in another allocation. PRRTE passes the id of the
// allocation it runs in.
func checkJobID(opt *options, own int64) error {
	if opt.jobID == "" {
		return nil
	}
	id, err := strconv.ParseInt(strings.TrimSpace(opt.jobID), 10, 64)
	if err != nil || id != own || own < 0 {
		return fmt.Errorf("srun: error: --jobid=%s: the shim starts steps only inside the job srun runs in", opt.jobID)
	}
	return nil
}
