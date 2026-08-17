package fabricator

import (
	"context"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// discoverGPUs populates each host's granted device list per the configured
// discovery method (REQ-GPU-001). It is best-effort: any failure is reported as
// a warning and leaves the affected hosts GPU-less, because discovery failure is
// non-fatal unless task_policy is gpu (enforced later, REQ-ENC-005). jobID is
// the GE job number ("" or "0" when unknown).
func discoverGPUs(ctx context.Context, r gedata.Runner, cfg *config.Config, e envReader, jobID string, hosts []nodeInfo) []string {
	if len(hosts) == 0 {
		return nil
	}
	switch cfg.GPU.Discovery {
	case "", "none", "off":
		return nil
	case "nvidia-smi":
		return discoverNvidiaSMI(ctx, r, hosts)
	default: // "qstat-gres" and any unknown method fall back to the primary source.
		return discoverQstatGres(ctx, r, cfg, e, jobID, hosts)
	}
}

// discoverQstatGres reads the granted RSMAP from `qstat -xml -j` and matches
// each granted host to a layout node. When no Runner is available (wrapper mode
// without exec) it falls back to SGE_HGR_<complex> for a single-node job only,
// since that variable is not multi-host-safe (SI-19).
func discoverQstatGres(ctx context.Context, r gedata.Runner, cfg *config.Config, e envReader, jobID string, hosts []nodeInfo) []string {
	complexName := cfg.GPU.GresComplex
	if complexName == "" {
		complexName = "gpu"
	}

	if r == nil || jobID == "" || jobID == "0" {
		if len(hosts) == 1 {
			if v := e.get("SGE_HGR_" + complexName); v != "" {
				hosts[0].gpus = gedata.ParseSGEHGR(v)
			}
		}
		return nil
	}

	granted, err := gedata.GrantedGPUs(ctx, r, jobID, complexName)
	if err != nil {
		// The XML view failed; try the plain resource_map view before giving up
		// (REQ-FAB-003 failure tolerance).
		granted, err = gedata.GrantedGPUsPlain(ctx, r, jobID, complexName)
		if err != nil {
			return []string{"GPU discovery failed (" + err.Error() + "); continuing without GPU environment"}
		}
	}
	var warns []string
	for _, g := range granted {
		matched := false
		for i := range hosts {
			if matchHost(g.Host, hosts[i]) {
				hosts[i].gpus = g.Devices
				matched = true
			}
		}
		if !matched {
			warns = append(warns, "granted GPUs on host "+g.Host+" did not match any allocation node")
		}
	}
	return warns
}

// discoverNvidiaSMI queries physical GPUs on the local host. nvidia-smi reports
// installed devices, not the GE-granted subset, so it is only correct for
// exclusive-host allocations; it also cannot see remote hosts. Both facts are
// surfaced as warnings (REQ-GPU-001).
func discoverNvidiaSMI(ctx context.Context, r gedata.Runner, hosts []nodeInfo) []string {
	warns := []string{"nvidia-smi discovery reports physical (not GE-granted) GPUs; valid only for exclusive-host allocations"}
	if len(hosts) > 1 {
		return append(warns, "nvidia-smi cannot enumerate remote hosts; GPU environment limited to the master node")
	}
	if r == nil {
		return warns
	}
	stdout, _, exit, err := r.Run(ctx, "nvidia-smi", "--query-gpu=index", "--format=csv,noheader")
	if err != nil || exit != 0 {
		return append(warns, "nvidia-smi failed; continuing without GPU environment")
	}
	hosts[0].gpus = parseNvidiaIndices(string(stdout))
	return warns
}

func parseNvidiaIndices(out string) []int {
	var ids []int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if n, err := strconv.Atoi(line); err == nil {
			ids = append(ids, n)
		}
	}
	return ids
}

// matchHost reports whether a granted-resource host name refers to layout node
// h. GE may report the short name or FQDN, so compare both, short-to-short.
func matchHost(grantedHost string, h nodeInfo) bool {
	g := shortHost(grantedHost)
	return g == shortHost(h.Name) || g == shortHost(h.FQDN)
}

func shortHost(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}
