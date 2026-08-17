// Package plan computes rank placement and task geometry from an allocation.
// This file implements the N3 task policy (spec section 8.3), which maps GE
// slot and GPU grants to SLURM task counts per the site's per-PE semantics.
package plan

import (
	"errors"
	"fmt"
)

// Policy names the task-count semantics for a PE (spec section 8.3).
type Policy string

const (
	// PolicySlot yields one task per slot (MPI-style).
	PolicySlot Policy = "slot"
	// PolicyNode yields one task per node.
	PolicyNode Policy = "node"
	// PolicyGPU yields one task per granted GPU.
	PolicyGPU Policy = "gpu"
)

// NodeAlloc is one node's grant, in nodelist order.
type NodeAlloc struct {
	Slots int
	GPUs  int
}

// TaskGeometry is the job-level task shape derived by a policy.
type TaskGeometry struct {
	// PerNode holds the task count for each node, in nodelist order. It feeds
	// the SLURM_TASKS_PER_NODE encoder (N2) and rank placement.
	PerNode []int
	// NTasks is the total task count (SLURM_NTASKS).
	NTasks int
	// CPUsPerTask is the derived SLURM_CPUS_PER_TASK. It is omitted from the
	// job environment when CPUsPerTaskSet is false (policy "slot", spec A11).
	CPUsPerTask    int
	CPUsPerTaskSet bool
	// Warnings holds non-fatal advisories (e.g. heterogeneous slots collapsed
	// to a minimum, REQ-ENC-004).
	Warnings []string
}

// ErrNoGPUs is returned by the gpu policy when no node was granted a GPU
// (REQ-ENC-005); the fabricator maps it to a job failure.
var ErrNoGPUs = errors.New("task_policy gpu but no node was granted a GPU")

// ApplyPolicy computes the task geometry for a policy over the allocation's
// per-node grants in nodelist order.
func ApplyPolicy(p Policy, nodes []NodeAlloc) (TaskGeometry, error) {
	if len(nodes) == 0 {
		return TaskGeometry{}, errors.New("no nodes in allocation")
	}
	switch p {
	case PolicySlot:
		return slotGeometry(nodes), nil
	case PolicyNode:
		return nodeGeometry(nodes), nil
	case PolicyGPU:
		return gpuGeometry(nodes)
	default:
		return TaskGeometry{}, fmt.Errorf("unknown task policy %q", p)
	}
}

// slotGeometry: one task per slot; cpus_per_task omitted (spec section 8.3).
func slotGeometry(nodes []NodeAlloc) TaskGeometry {
	g := TaskGeometry{PerNode: make([]int, len(nodes))}
	for i, n := range nodes {
		g.PerNode[i] = n.Slots
		g.NTasks += n.Slots
	}
	return g
}

// nodeGeometry: one task per node; cpus_per_task = per-node slots, collapsed to
// the minimum with a warning when heterogeneous (REQ-ENC-004).
func nodeGeometry(nodes []NodeAlloc) TaskGeometry {
	g := TaskGeometry{PerNode: make([]int, len(nodes)), NTasks: len(nodes)}
	minSlots := nodes[0].Slots
	uniform := true
	for i, n := range nodes {
		g.PerNode[i] = 1
		if n.Slots != nodes[0].Slots {
			uniform = false
		}
		if n.Slots < minSlots {
			minSlots = n.Slots
		}
	}
	g.CPUsPerTask = minSlots
	g.CPUsPerTaskSet = true
	if !uniform {
		g.Warnings = append(g.Warnings,
			fmt.Sprintf("heterogeneous slot counts; SLURM_CPUS_PER_TASK collapsed to minimum %d", minSlots))
	}
	return g
}

// gpuGeometry: one task per granted GPU; cpus_per_task = floor(slots/gpus) per
// node collapsed to the minimum with a warning when heterogeneous. Nodes with
// no GPU contribute zero tasks; an all-zero allocation is an error
// (REQ-ENC-004, REQ-ENC-005).
func gpuGeometry(nodes []NodeAlloc) (TaskGeometry, error) {
	g := TaskGeometry{PerNode: make([]int, len(nodes))}
	minCPT := -1
	for i, n := range nodes {
		g.PerNode[i] = n.GPUs
		g.NTasks += n.GPUs
		if n.GPUs == 0 {
			continue
		}
		cpt := n.Slots / n.GPUs
		if minCPT < 0 || cpt < minCPT {
			minCPT = cpt
		}
	}
	if g.NTasks == 0 {
		return TaskGeometry{}, ErrNoGPUs
	}
	g.CPUsPerTask = minCPT
	g.CPUsPerTaskSet = true

	first := -1
	uniform := true
	for _, n := range nodes {
		if n.GPUs == 0 {
			continue
		}
		cpt := n.Slots / n.GPUs
		if first < 0 {
			first = cpt
		} else if cpt != first {
			uniform = false
		}
	}
	if !uniform {
		g.Warnings = append(g.Warnings,
			fmt.Sprintf("heterogeneous slots-per-gpu; SLURM_CPUS_PER_TASK collapsed to minimum %d", minCPT))
	}
	return g, nil
}
