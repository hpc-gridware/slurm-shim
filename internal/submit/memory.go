package submit

import (
	"fmt"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// addressSpaceComplexes are the Grid Engine memory complexes enforced as a
// virtual ADDRESS SPACE cap (RLIMIT_AS) rather than as resident memory.
//
// Verified on OCS 9.1.5: a job submitted `-l h_vmem=1G` runs with
// `ulimit -v 1048576`, and an allocation past that fails ("memory exhausted").
// A CUDA context reserves tens of GB of address space at initialisation and
// touches almost none of it, so a --mem request mapped onto one of these kills
// every GPU process at start. Kept as a set, not a single name, because the
// s_/h_ pairs and h_data behave the same way.
var addressSpaceComplexes = map[string]bool{
	"h_vmem": true, "s_vmem": true,
	"h_data": true, "s_data": true,
	"h_as": true, "s_as": true,
}

// memoryComplexGPUWarn is the wording both sbatch and interactive srun print, so
// the guidance cannot drift between the commands (same rule as the -par
// warnings above).
const memoryComplexGPUWarn = "memory_complex %q is enforced as virtual address space " +
	"(RLIMIT_AS), but this job requests GPUs: a CUDA context reserves tens of GB of " +
	"address space at init, so --mem will fail the job before your code runs -- set " +
	"memory_complex to mem_free (see README: Memory requests)"

// MemoryComplexWarning reports the warning to print for a job's memory request,
// or "" when there is nothing to say.
//
// It fires only when all three hold: the job actually asked for memory, the
// site's complex is address-space enforced, and the job requests GPUs. A CPU-only
// site running h_vmem deliberately is a legitimate choice and stays quiet -- the
// warning names a failure that only materialises with a CUDA context.
func MemoryComplexWarning(cfg *config.Config, r Request) string {
	if r.Mem == "" || cfg.MemoryComplex == "" {
		return ""
	}
	if !addressSpaceComplexes[cfg.MemoryComplex] {
		return ""
	}
	if !GPURequested(r) {
		return ""
	}
	return fmt.Sprintf(memoryComplexGPUWarn, cfg.MemoryComplex)
}

// GPURequested reports whether the request asks for GPUs by any route.
func GPURequested(r Request) bool {
	return r.HaveGPUs && r.GPUs > 0
}

// loadSensorComplexes are memory complexes whose value comes from the execd's
// reported load (`qhost -F` shows them as `hl:`), not from a static
// `complex_values` capacity.
//
// They matter because `qsub -w e` verifies a request against an idle cluster and
// does not consider load values, so it refuses ANY request naming one. Verified
// on OCS 9.1.5:
//
//	qsub -w e -l mem_free=100M     -> "no suitable queues" (refused)
//	qsub -w e -l virtual_free=100M -> "no suitable queues" (refused)
//	qsub -w e -l h_vmem=100M       -> accepted
//	qsub -w e                      -> accepted
//
// This is the false refusal the geometry diagnostic in sbatch already warns
// about ("a complex your site populates only from a load sensor").
var loadSensorComplexes = map[string]bool{
	"mem_free": true, "virtual_free": true, "swap_free": true,
}

// VerifyGeometry reports whether `-w e` may ride along with `-par` for this
// request. It must not when the job also names a load-sensor memory complex:
// -w e cannot see load values, so it would refuse a job that runs fine, turning
// every `--nodes N --mem X` submission into a hard error at submit.
//
// The layout is still pinned with -par; only the submit-time pre-check is
// dropped, so an impossible layout waits in the queue instead of being rejected
// early. That is the strictly better failure mode of the two.
func VerifyGeometry(cfg *config.Config, r Request) bool {
	if r.Mem == "" || cfg.MemoryComplex == "" {
		return true
	}
	return !loadSensorComplexes[cfg.MemoryComplex]
}
