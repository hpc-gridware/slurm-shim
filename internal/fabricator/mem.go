package fabricator

import (
	"context"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

const bytesPerMB = 1024 * 1024

// discoverMemory computes SLURM_MEM_PER_NODE in MB from the job's requested
// memory complex (A27, SI-30), floored to MB. It is optional and
// failure-tolerant (REQ-FAB-003): disabled when memory_complex is empty,
// skipped (with a warning) when qstat fails or times out, and 0 when the job
// requested no memory. slots is the master node's slot count (v1 uses a single
// per-node value).
//
// The request is multiplied by slots ONLY when the complex is a per-slot
// consumable, which is what makes the product a per-node figure. Under any other
// scope -- including `consumable NO`, which every stock OCS 9.1.5 memory complex
// uses, mem_free included -- Grid Engine does no per-slot multiplication, and
// multiplying anyway reported several times the memory the job asked for
// (verified: --mem=100M with 4 tasks announced SLURM_MEM_PER_NODE=400).
func discoverMemory(ctx context.Context, r gedata.Runner, cfg *config.Config, jobID string, slots int) (int, []string) {
	if cfg.MemoryComplex == "" || r == nil || jobID == "" || jobID == "0" || slots <= 0 {
		return 0, nil
	}
	requested, ok, err := gedata.RequestedResource(ctx, r, jobID, cfg.MemoryComplex)
	if err != nil {
		return 0, []string{"memory discovery failed (" + err.Error() + "); omitting SLURM_MEM_PER_NODE"}
	}
	if !ok || requested <= 0 {
		return 0, nil
	}
	var warns []string
	scope, err := gedata.ConsumableScope(ctx, r, cfg.MemoryComplex)
	if err != nil {
		// Fall back to the unmultiplied request: correct for every scope except a
		// per-slot consumable, and understating the figure is safer than a job
		// believing it has several times the memory it asked for.
		warns = append(warns, "could not read the consumable scope of "+cfg.MemoryComplex+
			" ("+err.Error()+"); SLURM_MEM_PER_NODE reports the request as-is, which is "+
			"low if that complex is a per-slot consumable")
	}
	mb := int((int64(requested) * int64(gedata.PerNodeMultiplier(scope, slots))) / bytesPerMB)
	return mb, warns
}
