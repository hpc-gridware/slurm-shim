package submit

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// addressSpaceComplexes are the Grid Engine memory complexes enforced as a
// virtual ADDRESS SPACE cap (RLIMIT_AS) rather than as resident memory.
//
// Verified on OCS 9.1.5: a job submitted `-l h_vmem=1G` runs with
// `ulimit -v 1048576`, and an allocation past that fails ("memory exhausted").
// A GPU context reserves tens of GB of address space at initialisation and
// touches almost none of it, so a --mem request mapped onto one of these kills
// every GPU process at start. This is a property of the runtime reserving
// address space, not of any one vendor: CUDA and ROCm both do it. Kept as a set, not a single name, because the
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
	"(RLIMIT_AS), but this job requests GPUs: a GPU context reserves tens of GB of " +
	"address space at init, so --mem will fail the job before your code runs -- set " +
	"memory_complex to mem_free (see README: Memory requests)"

// MemoryComplexWarning reports the warning to print for a job's memory request,
// or "" when there is nothing to say.
//
// It fires only when all three hold: the job actually asked for memory, the
// site's complex is address-space enforced, and the job requests GPUs. A CPU-only
// site running h_vmem deliberately is a legitimate choice and stays quiet -- the
// warning names a failure that only materialises with a GPU context.
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

// PerUnitMemory is a --mem-per-cpu or --mem-per-gpu request: Value per unit
// (GE-formatted, "2G"), with the job shape that turns it into the one figure
// a memory complex takes.
type PerUnitMemory struct {
	Value string
	// UnitsPerNode is the CPUs (slots) or GPUs on one node, UnitsTotal those
	// of the whole job, SlotsPerNode the slots on one node.
	UnitsPerNode, UnitsTotal, SlotsPerNode int
}

// MemoryForScope converts a per-CPU or per-GPU memory request into the value
// to request on a memory complex with the given consumable scope. The same
// number means different things per scope (see gedata.ConsumableScope):
// per slot (YES), per job (JOB), or per host (HOST, and NO, where the value
// filters hosts on free memory). An unknown scope is treated as per host, the
// stock case, which over-asks rather than under-asks on a per-slot complex.
func MemoryForScope(m PerUnitMemory, scope string) (string, error) {
	mb, err := memMB(m.Value)
	if err != nil {
		return "", err
	}
	var total float64
	switch strings.ToUpper(scope) {
	case gedata.ConsumablePerSlot:
		if m.SlotsPerNode < 1 {
			return "", fmt.Errorf("cannot spread %s over an unknown slot count", m.Value)
		}
		total = mb * float64(m.UnitsPerNode) / float64(m.SlotsPerNode)
	case gedata.ConsumablePerJob:
		total = mb * float64(m.UnitsTotal)
	default:
		total = mb * float64(m.UnitsPerNode)
	}
	return formatMB(int64(math.Ceil(total))), nil
}

// memMB reads a GE-formatted memory value ("512M", "1.5G") as MB.
func memMB(value string) (float64, error) {
	b, ok := gedata.MemBytes(value)
	if !ok {
		return 0, fmt.Errorf("invalid memory value %q", value)
	}
	return b / (1 << 20), nil
}

// formatMB renders MB in the largest whole unit, so 4096 reads as 4G.
func formatMB(mb int64) string {
	if mb > 0 && mb%1024 == 0 {
		return strconv.FormatInt(mb/1024, 10) + "G"
	}
	return strconv.FormatInt(mb, 10) + "M"
}

// ScalePerUnitMemory turns a --mem-per-cpu or --mem-per-gpu request (kind
// names which) into the one value the memory complex takes, by the complex's
// consumable scope in cat. slots and nodes are the job's Grid Engine slot
// count and node count. CPUs are SLURM's (tasks x cpus-per-task), not slots:
// a partition with a fixed slot count grants more slots than the job has
// CPUs. nodes < 1 means the spread over hosts is not known. The warning is
// set when the scope could not be read or the spread had to be guessed.
func ScalePerUnitMemory(cfg *config.Config, cat gedata.Catalog, r Request, kind string, slots, nodes int) (string, string, error) {
	spreadUnknown := nodes < 1 && !r.HavePerNode
	nodes = max(nodes, 1)
	ntasks, cpt := r.ResolveGeometry()
	m := PerUnitMemory{
		Value:        r.Mem,
		UnitsPerNode: r.TasksOnNode(nodes) * cpt,
		UnitsTotal:   ntasks * cpt,
		SlotsPerNode: max(slots/nodes, 1),
	}
	if kind == "mem-per-gpu" {
		gpus, ok := GPURequest(r)
		if !ok || gpus < 1 {
			return "", "", fmt.Errorf("--mem-per-gpu needs a GPU request (--gpus-per-node, --gpus-per-task or --gres=gpu)")
		}
		m.UnitsPerNode, m.UnitsTotal = gpus, gpus*nodes
	}
	scope, warn := "", ""
	if cx, ok := cat.Lookup(cfg.MemoryComplex); ok {
		scope = cx.Consumable
	} else {
		warn = fmt.Sprintf("could not read how %s is consumed; --%s is requested per node", cfg.MemoryComplex, kind)
	}
	// A per-host complex needs the CPUs on one host, which nothing pins when
	// neither --nodes nor --ntasks-per-node is given: Grid Engine may put one
	// task or all of them on a host. Asking for every CPU's memory on each host
	// would leave a wide job waiting forever, so one task's worth is asked.
	perHost := !strings.EqualFold(scope, gedata.ConsumablePerSlot) && !strings.EqualFold(scope, gedata.ConsumablePerJob)
	if kind != "mem-per-gpu" && spreadUnknown && perHost && m.UnitsPerNode > cpt {
		m.UnitsPerNode = cpt
		warn = appendSentence(warn, fmt.Sprintf("--%s is requested for one task (%d CPU(s)) per host: "+
			"without --nodes or --ntasks-per-node the tasks per host are not known", kind, cpt))
	}
	v, err := MemoryForScope(m, scope)
	if err != nil {
		return "", "", fmt.Errorf("--%s: %w", kind, err)
	}
	return v, warn, nil
}

// appendSentence joins two warnings into one line.
func appendSentence(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// NodesForList sizes a job by a -w host list, less the -x hosts. SLURM runs
// on exactly the listed nodes, which Grid Engine can only do with the node
// count pinned to the list's length. A smaller --nodes is SLURM's own error;
// a larger one would let SLURM add other hosts, which a Grid Engine host list
// cannot, so the job would wait forever for nodes it may not use.
func NodesForList(nodes int, list, exclude []string) (int, error) {
	usable := UsableHosts(list, exclude)
	switch {
	case len(list) == 0:
		return nodes, nil
	case len(usable) == 0:
		return 0, fmt.Errorf("--nodelist and --exclude leave no host to run on")
	case nodes == 0:
		return len(usable), nil
	case nodes < len(usable):
		return 0, fmt.Errorf("--nodes=%d is fewer than the %d host(s) in --nodelist", nodes, len(usable))
	case nodes > len(usable):
		return 0, fmt.Errorf("--nodes=%d is more than the %d host(s) in --nodelist: Grid Engine cannot add "+
			"other hosts to a host list; name all %d hosts or drop --nodelist", nodes, len(usable), nodes)
	}
	return nodes, nil
}

// UsableHosts is a -w host list less the -x hosts.
func UsableHosts(list, exclude []string) []string {
	var keep []string
	for _, h := range list {
		if !slices.Contains(exclude, h) {
			keep = append(keep, h)
		}
	}
	return keep
}
