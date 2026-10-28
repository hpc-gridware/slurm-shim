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

// Package cpubind decides where a step's tasks may run on a host: which CPUs
// the job may use there (the affinity mask the stepper inherits from Grid
// Engine binding or a cgroup) and whether pinning each task is safe at all.
//
// srun plans each task's CPUs as positions in that allowed set ("0-1" = the
// first two allowed CPUs, see Node), never as CPU numbers: on a node shared with other
// jobs, or under Grid Engine core binding, CPU 0 is not the job's to take.
package cpubind

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Binding modes for a step, from srun --cpu-bind / SLURM_CPU_BIND.
const (
	// ModeAuto pins only where it cannot take CPUs from another job: within a
	// restricted mask, or on a node the job holds whole (see ShouldPin).
	ModeAuto = ""
	// ModeNone never pins (--cpu-bind=none).
	ModeNone = "none"
	// ModeExplicit pins within the job's CPUs even on a shared node: the user
	// asked for binding (--cpu-bind=cores, threads, rank, map_cpu:...).
	ModeExplicit = "explicit"
)

// ModeFor maps a --cpu-bind value to a binding mode. Qualifiers (verbose,
// quiet) select no type; "none" anywhere wins, as in SLURM.
func ModeFor(spec string) string {
	mode := ModeAuto
	for _, part := range strings.Split(spec, ",") {
		switch strings.TrimSpace(part) {
		case "none":
			return ModeNone
		case "", "verbose", "quiet", "v", "q":
		default:
			mode = ModeExplicit
		}
	}
	return mode
}

// Node is what a node offers a step's tasks. Allowed are the CPUs the job may
// use there (the inherited affinity mask), one hardware thread per core first,
// so consecutive positions land on different cores before two share one.
// Cores counts the cores they span and Online the node's online CPUs; either
// is 0 when unknown.
type Node struct {
	Allowed []int
	Cores   int
	Online  int
}

// Restricted reports a mask narrower than the node: Grid Engine binding or a
// cgroup gave the job these CPUs.
func (n Node) Restricted() bool { return n.Online > 0 && len(n.Allowed) < n.Online }

// Owned reports whether the job holds the node whole: at least one slot per
// core. Cores, not hardware threads: sites commonly give an SMT host one slot
// per core. A queue with more slots than cores can make two jobs look like
// owners of one node; that case stays pinned, as before.
func (n Node) Owned(nodeSlots int) bool {
	cores := n.Cores
	if cores == 0 {
		cores = len(n.Allowed)
	}
	return nodeSlots >= cores
}

// ShouldPin reports whether a node's tasks get their own CPUs: never under
// ModeNone or without CPU facts, always under ModeExplicit, and in auto mode
// within a restricted mask or on a node the job owns. On a node shared with
// other jobs and no binding, any CPU chosen could be another job's.
func ShouldPin(mode string, n Node, nodeSlots int) bool {
	switch {
	case mode == ModeNone || len(n.Allowed) == 0:
		return false
	case mode == ModeExplicit:
		return true
	default:
		return n.Restricted() || n.Owned(nodeSlots)
	}
}

// openMPIBindingVars stop OpenMPI from binding its ranks itself: OpenMPI 4.1
// reads the first, OpenMPI 5 (PRRTE) the second, and each ignores the other.
// OpenMPI binds to cores 0, 1, ... (or a whole socket): it ignores a mask
// Grid Engine binding set without a cgroup, and on a shared node it takes
// cores another job uses (CS-2945). With policy "none" the ranks inherit the
// job's CPUs.
var openMPIBindingVars = []string{"OMPI_MCA_hwloc_base_binding_policy", "PRTE_MCA_hwloc_default_binding_policy"}

// OpenMPIOverride returns the OpenMPI binding override ("NAME=none") for a
// job or step on nodes holding nodeSlots slots each. OpenMPI picks its binding
// once, in mpirun, for every node, so this is one decision, judged with the
// facts of the node mpirun runs on (Grid Engine binding is job-wide): none
// under a restricted mask (OpenMPI would bind outside it) and wherever srun
// would not pin a node's tasks itself (ShouldPin). A job owning every node,
// or asking for binding, keeps OpenMPI's per-core binding and its speed. A
// variable already set (getenv) is the user's choice: nothing is returned.
// Without CPU facts (non-Linux) neither.
func OpenMPIOverride(mode string, master Node, nodeSlots []int, getenv func(string) string) []string {
	for _, name := range openMPIBindingVars {
		if getenv(name) != "" {
			return nil
		}
	}
	if len(master.Allowed) == 0 {
		return nil
	}
	pinned := !master.Restricted()
	for _, slots := range nodeSlots {
		pinned = pinned && ShouldPin(mode, master, slots)
	}
	if pinned {
		return nil
	}
	out := make([]string, 0, len(openMPIBindingVars))
	for _, name := range openMPIBindingVars {
		out = append(out, name+"=none")
	}
	return out
}

// selectCPUs maps a cpuset of positions in allowed ("2-3") onto the CPU
// numbers at those positions. ok is false when a position lies beyond the
// allowed set or the cpuset does not parse.
func selectCPUs(positions string, allowed []int) (cpus string, ok bool) {
	idx, err := ParseList(positions)
	if err != nil {
		return "", false
	}
	picked := make([]int, 0, len(idx))
	for _, i := range idx {
		if i >= len(allowed) {
			return "", false
		}
		picked = append(picked, allowed[i])
	}
	return FormatList(picked), true
}

// Resolve turns a node's planned task cpusets (positions, "" = unpinned) into
// the CPU lists to apply, all "" when the node's tasks are not pinned. All or
// nothing: a mask too small for every task pins none, and the warning says so.
func Resolve(mode string, positions []string, n Node, nodeSlots int) (cpus []string, warning string) {
	cpus = make([]string, len(positions))
	if !ShouldPin(mode, n, nodeSlots) {
		return cpus, ""
	}
	for i, p := range positions {
		if p == "" {
			continue
		}
		c, ok := selectCPUs(p, n.Allowed)
		if !ok {
			return make([]string, len(positions)), fmt.Sprintf(
				"the job may use %d CPU(s) here (%s), too few for its tasks; tasks run unpinned",
				len(n.Allowed), FormatList(n.Allowed))
		}
		cpus[i] = c
	}
	return cpus, ""
}

// CoreFirst orders allowed (sorted CPU numbers) one hardware thread per core
// first, then the second threads, and so on, and counts the cores. coreOf maps
// a CPU to its core's key (its first sibling); CPUs it does not name are cores
// of their own.
func CoreFirst(allowed []int, coreOf map[int]int) (ordered []int, cores int) {
	var keys []int
	threads := map[int][]int{}
	for _, c := range allowed {
		key, ok := coreOf[c]
		if !ok {
			key = c
		}
		if _, seen := threads[key]; !seen {
			keys = append(keys, key)
		}
		threads[key] = append(threads[key], c)
	}
	for t := 0; len(ordered) < len(allowed); t++ {
		for _, key := range keys {
			if t < len(threads[key]) {
				ordered = append(ordered, threads[key][t])
			}
		}
	}
	return ordered, len(keys)
}

// ParseList parses a Linux/SLURM CPU list such as "0-3,5" into sorted,
// distinct CPU numbers.
func ParseList(s string) ([]int, error) {
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi := part, part
		if dash := strings.IndexByte(part, '-'); dash >= 0 {
			lo, hi = part[:dash], part[dash+1:]
		}
		first, err1 := strconv.Atoi(lo)
		last, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || first < 0 || last < first {
			return nil, fmt.Errorf("invalid cpu list %q", s)
		}
		for c := first; c <= last; c++ {
			seen[c] = true
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("empty cpu list %q", s)
	}
	out := make([]int, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Ints(out)
	return out, nil
}

// FormatList renders CPU numbers as a compact list ("0-3,5"), the form
// /proc/<pid>/status and taskset print.
func FormatList(cpus []int) string {
	sorted := append([]int(nil), cpus...)
	sort.Ints(sorted)
	var parts []string
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(sorted[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", sorted[i], sorted[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}
