package sbatch

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// resolveAllocationRule derives the allocation rule for a request and decides
// whether this cluster and this site's policy allow emitting it, surfacing any
// warning on stderr. An empty result means submit exactly as the shim did before
// -par existed.
//
// The capability probe runs only when a rule was actually derived, so a request
// that states no layout -- the majority of hand-written sbatch lines -- costs no
// extra process.
func resolveAllocationRule(runner gedata.Runner, cfg *config.Config, opt options,
	part config.Partition, slots int, stderr io.Writer) allocationRule {

	// Resolve the policy before printing anything. An explicit opt-out is quiet --
	// the site already knows -- and advice about how to pin a layout is worse than
	// useless when pinning is switched off for this partition.
	mode := cfg.AllocationRuleMode(part)
	if mode == config.OverrideNever {
		return allocationRule{}
	}

	spec := parSpec(opt, part, slots)
	if spec.Warn != "" {
		fmt.Fprintln(stderr, "sbatch: warning: "+spec.Warn)
	}
	if !spec.emit() {
		return allocationRule{}
	}
	if mode == config.OverrideAlways {
		return spec
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.QstatTimeout.Duration)
	defer cancel()
	supported, err := gedata.NewCapabilities(runner).AllocationRuleOverride(ctx)
	switch {
	case err != nil:
		// Distinct from "this cluster is too old": the usual cause is no Grid
		// Engine client on PATH, and telling that user to upgrade OCS sends them
		// somewhere they cannot fix it.
		fmt.Fprintf(stderr, "sbatch: warning: could not probe qsub for -par support (%v), so "+
			"--nodes/--ntasks-per-node are not enforced -- Grid Engine places the nodes via "+
			"the PE's allocation_rule\n", err)
		return allocationRule{}
	case !supported:
		fmt.Fprintln(stderr, "sbatch: warning: this cluster's qsub has no -par, so "+
			"--nodes/--ntasks-per-node are not enforced -- Grid Engine places the nodes via "+
			"the PE's allocation_rule (needs OCS 9.1.5 or newer; set "+
			"allocation_rule_override: never to silence)")
		return allocationRule{}
	}
	return spec
}

// verificationRejection is the marker in qsub's stderr for a `-w e` refusal. Grid
// Engine exits 1 for every submit failure, so the exit code alone cannot tell a
// geometry refusal from an unreachable qmaster, a bad queue name or a denied ACL;
// matching the message keeps the SLURM-shaped diagnostic off failures it does not
// explain.
const verificationRejection = "no suitable queues"

// isGeometryRejection reports whether qsub's stderr is the `-w e` verification
// refusal rather than some other submit failure.
func isGeometryRejection(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), verificationRejection)
}

// geometryRejection renders a `-w e` refusal the way SLURM renders an impossible
// geometry, and names both things that can cause it: a layout no host set can
// satisfy, and a complex the site populates only from a load sensor (which `-w e`
// evaluates against an empty cluster and therefore sees as absent).
func geometryRejection(part config.Partition, rule allocationRule, slots int, geMsg string) string {
	layout := fmt.Sprintf("%d node(s) with %d slot(s) each", rule.Nodes, slots/rule.Nodes)
	if rule.Value == "$pe_slots" {
		layout = fmt.Sprintf("1 node with all %d slot(s)", slots)
	}
	return fmt.Sprintf(`Requested node configuration is not available
  the request needs %s in queue %s (pe %s)
  Grid Engine reports: %s
  this is either a layout no host set can satisfy, or a complex your site
  populates only from a load sensor (qsub -w e evaluates an empty cluster, so it
  cannot see one); set allocation_rule_override: never to submit without
  enforcing the node layout`, layout, part.Queue, part.PE, firstLine(geMsg))
}

// geometryWarnings reports what a pinned layout makes visible that the shim could
// previously leave vague. Both are cheap and need no Grid Engine call.
//
// They only fire once a rule was emitted, because until the spread is pinned the
// shim genuinely does not know what the job will get, and a warning about a
// layout Grid Engine had not committed to would be noise.
func geometryWarnings(cfg *config.Config, opt options, part config.Partition, rule allocationRule) []string {
	if !rule.emit() {
		return nil
	}
	var warns []string
	ntasks, _ := opt.resolveGeometry()

	// resolveGeometry silently prefers --ntasks over a contradicting
	// --ntasks-per-node. That preference used to be invisible; now it decides
	// where the tasks physically land, so say so.
	if opt.haveNtasks && opt.havePerNode && opt.nodes >= 1 &&
		opt.ntasks != opt.nodes*opt.ntasksPerNode {
		warns = append(warns, fmt.Sprintf(
			"--ntasks %d contradicts --nodes %d x --ntasks-per-node %d (= %d); SLURM resolves "+
				"this in favour of --ntasks, so the layout pinned here is %d task(s) per node, "+
				"not the %d you asked for",
			opt.ntasks, opt.nodes, opt.ntasksPerNode, opt.nodes*opt.ntasksPerNode,
			ntasks/rule.Nodes, opt.ntasksPerNode))
	}

	// The spread is now exact, but SLURM_NTASKS still comes from the PE's task
	// policy. A node-policy partition yields one task per node whatever the
	// geometry says, so the report is about to look more authoritative than the
	// task count actually is.
	if opt.haveNtasks && taskPolicyFor(cfg, part) == "node" && rule.Nodes != opt.ntasks {
		warns = append(warns, fmt.Sprintf(
			"partition %q uses task_policy node, so the job will see SLURM_NTASKS=%d (one per "+
				"node), not the %d requested with --ntasks; the placement is pinned either way",
			opt.partition, rule.Nodes, opt.ntasks))
	}
	return warns
}

// taskPolicyFor resolves the task policy sbatch can see at submit time: the
// per-job override (which reaches the job through qsub -V, so it applies), then
// the PE's config entry, then the site default (SI-38).
func taskPolicyFor(cfg *config.Config, part config.Partition) string {
	if o := strings.TrimSpace(os.Getenv("SLURM_SHIM_TASK_POLICY")); o != "" {
		return o
	}
	if p, ok := cfg.PEs[part.PE]; ok && p.TaskPolicy != "" {
		return p.TaskPolicy
	}
	return cfg.DefaultTaskPolicy
}

// firstLine keeps a multi-line Grid Engine diagnostic from breaking the shape of
// the message it is quoted inside. qsub's refusal is "Unable to run job: <reason>"
// followed by "Exiting.", and only the first line carries the reason.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
