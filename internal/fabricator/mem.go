package fabricator

import (
	"context"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

const bytesPerMB = 1024 * 1024

// discoverMemory computes SLURM_MEM_PER_NODE in MB from the job's requested
// memory complex (A27, SI-30): per-slot request bytes x node slots, floored to
// MB. It is optional and failure-tolerant (REQ-FAB-003): disabled when
// memory_complex is empty, skipped (with a warning) when qstat fails or times
// out, and 0 when the job requested no memory. slots is the master node's slot
// count (v1 uses a single per-node value).
func discoverMemory(ctx context.Context, r gedata.Runner, cfg *config.Config, jobID string, slots int) (int, []string) {
	if cfg.MemoryComplex == "" || r == nil || jobID == "" || jobID == "0" || slots <= 0 {
		return 0, nil
	}
	bytesPerSlot, ok, err := gedata.RequestedResource(ctx, r, jobID, cfg.MemoryComplex)
	if err != nil {
		return 0, []string{"memory discovery failed (" + err.Error() + "); omitting SLURM_MEM_PER_NODE"}
	}
	if !ok || bytesPerSlot <= 0 {
		return 0, nil
	}
	mb := int((int64(bytesPerSlot) * int64(slots)) / bytesPerMB)
	return mb, nil
}
