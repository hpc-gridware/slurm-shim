// Package fabricator builds the SLURM environment contract (Table A) and the
// canonical layout file from a GE PE allocation (spec section 6). It is pure
// assembly over injected inputs (environment lookup, config, Identity), so the
// full Table A/B golden suites run without a cluster.
package fabricator

import (
	"context"
	"fmt"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
	"github.com/hpc-gridware/slurm-shim/internal/version"
)

// Env is an environment lookup (os.Getenv or a fixture map), injected so
// fabrication is deterministic under test.
type Env func(key string) string

// Options are the fabricator inputs.
type Options struct {
	Env      Env
	Config   *config.Config
	Identity gedata.Identity
	NowUnix  int64
	// Runner reaches the GE clients for GPU discovery (REQ-GPU-001). It is
	// optional: when nil, discovery is skipped (or, for a single-node job, uses
	// SGE_HGR_<complex> from the environment), so the Table A/B golden suites
	// still run without a cluster.
	Runner gedata.Runner
}

// KV is one ordered export.
type KV struct{ Key, Value string }

// Result is the fabricated contract: the canonical layout, the ordered unset
// preamble (REQ-ENV-011), the ordered Table A exports, and any warnings.
type Result struct {
	Layout   *layout.Layout
	Unset    []string
	Exports  []KV
	Warnings []string
	// Disabled is true when SLURM_SHIM_DISABLE was set: no layout or exports are
	// produced, only the unset preamble (scrub-only mode, SI-38/SI-56).
	Disabled bool
}

// Fabricate assembles Table A and the layout from the allocation. On
// SLURM_SHIM_DISABLE it returns a scrub-only result.
func Fabricate(opts Options) (*Result, error) {
	e := envReader{get: defaultEnv(opts.Env)}
	cfg := opts.Config
	if cfg == nil {
		cfg = config.Default()
	}

	res := &Result{Unset: unsetPreamble()}
	if e.get("SLURM_SHIM_DISABLE") != "" {
		res.Disabled = true
		return res, nil
	}

	nodes, err := resolveNodes(e, opts.Identity)
	if err != nil {
		return nil, err
	}

	// qstat-backed discovery (GPUs, memory) shares one timeout-bounded context
	// so a slow qmaster cannot stall fabrication (REQ-FAB-003).
	qctx, cancel := context.WithTimeout(context.Background(), cfg.QstatTimeout.Duration)
	defer cancel()

	// Discover granted GPUs before geometry, since the task policy reads the
	// per-node GPU count (REQ-GPU-001).
	res.Warnings = append(res.Warnings,
		discoverGPUs(qctx, opts.Runner, cfg, e, e.get("JOB_ID"), nodes.hosts)...)

	policy := taskPolicy(e, cfg, nodes.peName)
	allocs := make([]plan.NodeAlloc, len(nodes.hosts))
	for i, h := range nodes.hosts {
		allocs[i] = plan.NodeAlloc{Slots: h.Slots, GPUs: len(h.gpus)}
	}
	geom, err := plan.ApplyPolicy(policy, allocs)
	if err != nil {
		return nil, err
	}
	res.Warnings = append(res.Warnings, geom.Warnings...)

	jobID := e.int64("JOB_ID")
	taskID, hasTask := arrayTaskID(e)
	port := cfg.MasterPortBase + int((jobID*31+zeroIf(taskID, hasTask))%int64(cfg.MasterPortRange))

	id := resolveWho(opts.Identity)
	lay := buildLayout(opts.NowUnix, e, cfg, nodes, geom, policy, port, id)

	// Per-node memory (A27) is derived from the requested memory complex scaled
	// by the master node's slots (SI-30); optional and failure-tolerant.
	memMB, memWarns := discoverMemory(qctx, opts.Runner, cfg, e.get("JOB_ID"), nodes.hosts[0].Slots)
	lay.Job.MemPerNodeMB = memMB
	res.Warnings = append(res.Warnings, memWarns...)
	res.Layout = lay

	res.Exports = buildTableA(e, cfg, nodes, geom, lay, id, hasTask, taskID)

	if !uniform(geom.PerNode) {
		// A10 omitted; warn because Lightning hard-crashes without it (SI-40).
		res.Warnings = append(res.Warnings,
			"non-uniform per-node task counts: SLURM_NTASKS_PER_NODE omitted (Lightning requires it)")
	}

	if err := validate(res, geom); err != nil {
		return nil, err
	}
	return res, nil
}

// nodeSet is the parsed, IP-resolved allocation plus job-scoped metadata.
type nodeSet struct {
	hosts  []nodeInfo
	peName string
}

type nodeInfo struct {
	gedata.Host
	ip   string
	gpus []int
}

// resolveNodes parses PE_HOSTFILE (or fabricates a single-node layout when it is
// absent and NHOSTS<=1, SI-11) and resolves per-node IPs best-effort.
func resolveNodes(e envReader, id gedata.Identity) (nodeSet, error) {
	ns := nodeSet{peName: e.get("PE")}
	hostfile := e.get("PE_HOSTFILE")

	var hosts []gedata.Host
	if hostfile == "" && e.intDefault("NHOSTS", 1) <= 1 {
		// Single-node fallback: localhost with NSLOTS (or 1) slots.
		short := "localhost"
		if id != nil {
			if h, err := id.Hostname(); err == nil && h != "" {
				short = h
			}
		}
		hosts = []gedata.Host{{Name: short, FQDN: short, Slots: e.intDefault("NSLOTS", 1)}}
	} else {
		data, err := readHostfile(hostfile)
		if err != nil {
			return ns, err
		}
		hosts, err = gedata.ParsePEHostfile(data)
		if err != nil {
			return ns, err
		}
	}

	ns.hosts = make([]nodeInfo, len(hosts))
	for i, h := range hosts {
		ni := nodeInfo{Host: h}
		if id != nil {
			ni.ip, _ = id.LookupIP(context.Background(), h.Name)
		}
		ns.hosts[i] = ni
	}
	return ns, nil
}

// taskPolicy resolves the policy from the per-job override, then the PE's config
// entry, then the default (SI-38).
func taskPolicy(e envReader, cfg *config.Config, pe string) plan.Policy {
	if o := e.get("SLURM_SHIM_TASK_POLICY"); o != "" {
		return plan.Policy(o)
	}
	if p, ok := cfg.PEs[pe]; ok && p.TaskPolicy != "" {
		return plan.Policy(p.TaskPolicy)
	}
	if cfg.DefaultTaskPolicy != "" {
		return plan.Policy(cfg.DefaultTaskPolicy)
	}
	return plan.PolicyNode
}

// buildLayout assembles the canonical layout including the block rank map.
func buildLayout(now int64, e envReader, cfg *config.Config, ns nodeSet, geom plan.TaskGeometry, policy plan.Policy, port int, id who) *layout.Layout {
	nodes := make([]layout.Node, len(ns.hosts))
	for i, h := range ns.hosts {
		nodes[i] = layout.Node{
			Index:          i,
			Host:           h.Name,
			FQDN:           h.FQDN,
			IP:             h.ip,
			Slots:          h.Slots,
			ProcessorRange: h.ProcessorRange,
			GPUs:           h.gpus,
			IsMaster:       i == 0,
		}
	}

	taskID, hasTask := arrayTaskID(e)
	var arrayPtr *int64
	if hasTask {
		v := taskID
		arrayPtr = &v
	}

	return &layout.Layout{
		SchemaVersion: layout.SchemaVersion,
		ShimVersion:   version.Shim,
		CreatedUnix:   now,
		Job: layout.Job{
			JobID:       e.int64("JOB_ID"),
			ArrayTaskID: arrayPtr,
			Name:        sanitizeFreeText(e.get("JOB_NAME")),
			User:        id.user,
			UID:         id.uid,
			GID:         id.gid,
			Queue:       masterQueue(e, ns),
			Partition:   partition(e, cfg, ns),
			Account:     e.get("SGE_ACCOUNT"),
			SubmitDir:   submitDir(e),
			SubmitHost:  e.get("SGE_O_HOST"),
			PEName:      ns.peName,
			TaskPolicy:  string(policy),
		},
		Nodes:      nodes,
		Tasks:      buildTasks(geom),
		Rendezvous: layout.Rendezvous{MasterAddr: ns.hosts[0].Name, MasterPort: port},
		Launcher:   cfg.Launcher,
	}
}

// buildTasks builds the tasks block with a block-distribution rank map. cpuset
// is a contiguous per-rank slice sized by cpus_per_task. Per-rank GPU assignment
// is a step concern (REQ-GPU-002): the srun step planner partitions the node's
// granted devices at launch (plan.Place), so the job-level rank map carries only
// placement, not devices.
func buildTasks(geom plan.TaskGeometry) layout.Tasks {
	t := layout.Tasks{
		NTasks:      geom.NTasks,
		CPUsPerTask: geom.CPUsPerTask,
		PerNode:     geom.PerNode,
	}
	rank := 0
	for nodeIdx, count := range geom.PerNode {
		for local := 0; local < count; local++ {
			cpt := geom.CPUsPerTask
			if cpt <= 0 {
				cpt = 1
			}
			t.RankMap = append(t.RankMap, layout.Rank{
				Rank:   rank,
				Node:   nodeIdx,
				Local:  local,
				Cpuset: fmt.Sprintf("%d-%d", local*cpt, (local+1)*cpt-1),
			})
			rank++
		}
	}
	return t
}

// validate enforces the export invariants (REQ-FAB-006/007) before the result
// is returned.
func validate(res *Result, geom plan.TaskGeometry) error {
	nodelist := lookup(res.Exports, "SLURM_JOB_NODELIST")
	// (a) sum of N2 counts == ntasks.
	counts, err := encoders.ExpandCounts(lookup(res.Exports, "SLURM_TASKS_PER_NODE"))
	if err != nil {
		return fmt.Errorf("tasks-per-node self-check: %w", err)
	}
	sum := 0
	for _, c := range counts {
		sum += c
	}
	if sum != geom.NTasks {
		return fmt.Errorf("invariant: sum(tasks_per_node)=%d != ntasks=%d", sum, geom.NTasks)
	}
	// (b) node count in N1 nodelist == nnodes; (e) nodelist round-trips.
	expanded, err := encoders.ExpandNodelist(nodelist)
	if err != nil {
		return fmt.Errorf("nodelist self-check: %w", err)
	}
	if len(expanded) != len(res.Layout.Nodes) {
		return fmt.Errorf("invariant: nodelist expands to %d != nnodes=%d", len(expanded), len(res.Layout.Nodes))
	}
	// (c) machine values pass the charset whitelist.
	if !machineValueOK(nodelist) {
		return fmt.Errorf("invariant: nodelist %q fails the charset check", nodelist)
	}
	return nil
}

func readHostfile(path string) ([]byte, error) {
	return osReadFile(path)
}
