package plan

import (
	"fmt"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// StepRequest is the geometry an srun invocation asks for. Zero fields mean the
// flag was not given.
type StepRequest struct {
	Nodes        int      // -N
	NTasks       int      // -n
	TasksPerNode int      // --ntasks-per-node
	CPUsPerTask  int      // -c
	GPUsPerTask  int      // --gpus-per-task
	Nodelist     []string // -w, expanded host names
}

// StepNode is one host participating in a step, in step-nodelist order.
type StepNode struct {
	Host        string
	LayoutIndex int
	Slots       int
	GPUs        []int
}

// PlacedRank is one rank's placement (Table B geometry).
type PlacedRank struct {
	Rank          int
	StepNodeIndex int // SLURM_NODEID
	Local         int // SLURM_LOCALID
	Cpuset        string
	GPUs          []int
}

// StepPlan is the resolved placement for a step.
type StepPlan struct {
	Nodes       []StepNode
	Ranks       []PlacedRank
	NTasks      int
	CPUsPerTask int
	Warnings    []string
}

// Place resolves an srun request against the allocation into a concrete
// placement (spec sec. 7.2). It selects the step node set, determines the rank
// count, and block-distributes ranks (sec. 8.4), assigning cpusets and GPUs.
func Place(lay *layout.Layout, req StepRequest) (*StepPlan, error) {
	nodes, err := selectNodes(lay, req)
	if err != nil {
		return nil, err
	}

	perNodeCap := capacities(lay, nodes, req)
	ntasks := req.NTasks
	if ntasks == 0 {
		for _, c := range perNodeCap {
			ntasks += c
		}
	}

	cpt := req.CPUsPerTask
	if cpt <= 0 {
		cpt = lay.Tasks.CPUsPerTask
	}
	if cpt <= 0 {
		cpt = 1
	}

	ranks, warns, err := distribute(nodes, perNodeCap, ntasks, cpt, req.GPUsPerTask)
	if err != nil {
		return nil, err
	}
	return &StepPlan{Nodes: nodes, Ranks: ranks, NTasks: ntasks, CPUsPerTask: cpt, Warnings: warns}, nil
}

// selectNodes chooses the participating hosts in allocation order: the
// --nodelist subset (REQ-RUN-002), else the first -N nodes, else all nodes.
// A -w host outside the allocation is an error; the result is normalized to
// allocation order so SLURM_NODEID is deterministic (SI-26).
func selectNodes(lay *layout.Layout, req StepRequest) ([]StepNode, error) {
	toStep := func(idx int) StepNode {
		n := lay.Nodes[idx]
		return StepNode{Host: n.Host, LayoutIndex: idx, Slots: n.Slots, GPUs: n.GPUs}
	}

	if len(req.Nodelist) > 0 {
		want := map[string]bool{}
		for _, h := range req.Nodelist {
			want[h] = true
		}
		var out []StepNode
		for i, n := range lay.Nodes {
			if want[n.Host] {
				out = append(out, toStep(i))
				delete(want, n.Host)
			}
		}
		if len(want) > 0 {
			for h := range want {
				return nil, fmt.Errorf("srun: error: node %q is not part of the allocation", h)
			}
		}
		return out, nil
	}

	limit := len(lay.Nodes)
	if req.Nodes > 0 {
		if req.Nodes > limit {
			return nil, fmt.Errorf("srun: error: requested %d nodes, allocation has %d", req.Nodes, limit)
		}
		limit = req.Nodes
	}
	out := make([]StepNode, limit)
	for i := 0; i < limit; i++ {
		out[i] = toStep(i)
	}
	return out, nil
}

// capacities is the per-node task capacity in step order. --ntasks-per-node
// overrides it; when only -N is given the capacity is one task per node
// (SI-26); otherwise it is the layout policy capacity. The rank count then
// derives from these capacities (summed) unless -n overrode it.
func capacities(lay *layout.Layout, nodes []StepNode, req StepRequest) []int {
	onePerNode := req.Nodes > 0 && req.NTasks == 0 && req.TasksPerNode == 0
	caps := make([]int, len(nodes))
	for i, n := range nodes {
		switch {
		case req.TasksPerNode > 0:
			caps[i] = req.TasksPerNode
		case onePerNode:
			caps[i] = 1
		case n.LayoutIndex < len(lay.Tasks.PerNode):
			caps[i] = lay.Tasks.PerNode[n.LayoutIndex]
		default:
			caps[i] = n.Slots
		}
	}
	return caps
}

// distribute block-fills ranks node-by-node to capacity (sec. 8.4). Exceeding
// total capacity fails before any launch (REQ-RUN-008); a GPU request beyond a
// node's grant fails too (SI-31).
func distribute(nodes []StepNode, caps []int, ntasks, cpt, gpusPerTask int) ([]PlacedRank, []string, error) {
	total := 0
	for _, c := range caps {
		total += c
	}
	if ntasks > total {
		return nil, nil, fmt.Errorf(
			"srun: error: Unable to allocate resources: More processors requested than permitted (%d > %d)",
			ntasks, total)
	}

	var ranks []PlacedRank
	var warns []string
	rank := 0
	for ni := range nodes {
		// nlocal is the number of ranks actually placed on this node (the last
		// node may be truncated by ntasks).
		nlocal := caps[ni]
		if rank+nlocal > ntasks {
			nlocal = ntasks - rank
		}
		if nlocal <= 0 {
			break
		}

		// Each node's granted devices are partitioned contiguously among its
		// local ranks (REQ-GPU-002). An explicit --gpus-per-task that exceeds the
		// grant is an error; the flag-less default derives the per-rank count.
		if gpusPerTask > 0 && gpusPerTask*nlocal > len(nodes[ni].GPUs) {
			return nil, nil, fmt.Errorf(
				"srun: error: --gpus-per-task=%d exceeds the %d GPUs granted on %s",
				gpusPerTask, len(nodes[ni].GPUs), nodes[ni].Host)
		}
		gpuPerRank, shared := AssignDevices(nodes[ni].GPUs, nlocal, gpusPerTask)
		if shared && len(warns) == 0 {
			warns = append(warns, fmt.Sprintf(
				"srun: warning: %d GPUs but %d tasks on %s; all ranks share the node's GPUs",
				len(nodes[ni].GPUs), nlocal, nodes[ni].Host))
		}
		for local := 0; local < nlocal; local++ {
			ranks = append(ranks, PlacedRank{
				Rank:          rank,
				StepNodeIndex: ni,
				Local:         local,
				Cpuset:        fmt.Sprintf("%d-%d", local*cpt, (local+1)*cpt-1),
				GPUs:          gpuPerRank[local],
			})
			rank++
		}
	}
	return ranks, warns, nil
}
