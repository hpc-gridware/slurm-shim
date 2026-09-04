package submit

import (
	"context"
	"fmt"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// The two messages emitted when -par cannot be used. Shared verbatim by sbatch
// and interactive srun so the guidance about --nodes/--ntasks-per-node not being
// enforced cannot drift between the commands (todos/011).
const (
	parProbeFailedWarn = "could not probe qsub for -par support (%v), so " +
		"--nodes/--ntasks-per-node are not enforced -- Grid Engine places the nodes via " +
		"the PE's allocation_rule"
	parUnsupportedWarn = "this cluster's qsub has no -par, so " +
		"--nodes/--ntasks-per-node are not enforced -- Grid Engine places the nodes via " +
		"the PE's allocation_rule (needs OCS 9.1.5 or newer; set " +
		"allocation_rule_override: never to silence)"
)

// AllocationRuleProbe reports whether this cluster's qsub can honor `-par`, which
// is what decides whether a computed AllocationRule is actually emitted. The
// three outcomes match gedata.Capabilities: supported, ran-and-unsupported, and
// probe-failed; the last two carry the warning the caller prints. Both sbatch and
// interactive srun call this, so the OCS-version gate lives in one place.
//
// The warn uses a %v verb only in the probe-failed case; format it with the
// error there, and print it as-is otherwise.
func AllocationRuleProbe(ctx context.Context, r gedata.Runner) (supported bool, warn string) {
	ok, err := gedata.NewCapabilities(r).AllocationRuleOverride(ctx)
	switch {
	case err != nil:
		// Distinct from "too old": the usual cause is no GE client on PATH, and
		// telling that user to upgrade OCS sends them somewhere they cannot fix it.
		return false, fmtProbeErr(err)
	case !ok:
		return false, parUnsupportedWarn
	default:
		return true, ""
	}
}

// fmtProbeErr renders the probe-failed warning with the error.
func fmtProbeErr(err error) string {
	return fmt.Sprintf(parProbeFailedWarn, err)
}

// AllocationRuleFor computes the layout-pinning rule for a request on a partition
// and applies the site's override mode plus the -par capability probe, returning
// the rule to emit (empty AllocationRule when none) and any warnings to print.
// This is the single resolver both sbatch and interactive srun use.
func AllocationRuleFor(ctx context.Context, r gedata.Runner, cfg *config.Config,
	req Request, part config.Partition, slots int) (AllocationRule, []string) {

	mode := cfg.AllocationRuleMode(part)
	if mode == config.OverrideNever {
		return AllocationRule{}, nil // opted out: quiet, and no layout advice
	}
	spec := ParSpec(req, part, slots)
	var warns []string
	if spec.Warn != "" {
		warns = append(warns, spec.Warn)
	}
	if !spec.Emit() {
		return AllocationRule{}, warns
	}
	if mode == config.OverrideAlways {
		return spec, warns
	}
	supported, warn := AllocationRuleProbe(ctx, r)
	if warn != "" {
		warns = append(warns, warn)
	}
	if !supported {
		return AllocationRule{}, warns
	}
	return spec, warns
}
