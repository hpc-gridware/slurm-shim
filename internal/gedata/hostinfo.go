package gedata

import (
	"context"
	"sort"
	"strings"

	qconf "github.com/hpc-gridware/go-clusterscheduler/pkg/qconf/core"
)

// PerSlotMemoryLimits are the queue rlimits Grid Engine applies PER SLOT, and
// therefore the ones that make the daemon_forks_slaves setting dangerous. Time
// and file limits are deliberately absent: they are not what multiplies, and
// SI-18 is a memory story.
var PerSlotMemoryLimits = []string{
	"h_vmem", "s_vmem",
	"h_rss", "s_rss",
	"h_data", "s_data",
	"h_stack", "s_stack",
}

// MemoryLimitSet reports whether a queue limit value caps anything: every
// value but GE's INFINITY. A default INFINITY with a per-host override
// ("INFINITY,[n1=4G]") counts as set -- the safe direction.
func MemoryLimitSet(value string) bool {
	v := strings.TrimSpace(value)
	return v != "" && !strings.EqualFold(v, "INFINITY")
}

// queueMemoryLimits renders the per-slot memory limits a queue sets as
// sorted "name=value" entries, from the library's parsed queue.
func queueMemoryLimits(c qconf.ClusterQueueConfig) []string {
	byName := map[string][]string{
		"h_vmem": c.HVmem, "s_vmem": c.SVmem,
		"h_rss": c.HRss, "s_rss": c.SRss,
		"h_data": c.HData, "s_data": c.SData,
		"h_stack": c.HStack, "s_stack": c.SStack,
	}
	var out []string
	for _, name := range PerSlotMemoryLimits {
		if v := strings.Join(byName[name], ","); MemoryLimitSet(v) {
			out = append(out, name+"="+v)
		}
	}
	sort.Strings(out)
	return out
}

// ClusterConf is the part of the global or a host-local configuration the
// shim reads. List values are joined with commas; "" when unset.
type ClusterConf struct {
	QmasterParams string
	ExecdParams   string
	RshDaemon     string
	ExecdSpoolDir string
}

// GlobalConf reads the global configuration (qconf -sconf).
func (a *Admin) GlobalConf(ctx context.Context) (ClusterConf, error) {
	g, err := a.q.ShowGlobalConfiguration()
	if err != nil {
		return ClusterConf{}, err
	}
	return ClusterConf{
		QmasterParams: strings.Join(dropNone(g.QmasterParams), ","),
		ExecdParams:   strings.Join(dropNone(g.ExecdParams), ","),
		RshDaemon:     g.RshDaemon,
		ExecdSpoolDir: g.ExecdSpoolDir,
	}, nil
}

// HostsWithLocalConf lists the hosts that have a local configuration (qconf
// -sconfl). Every other host runs on the global configuration alone, so its
// local one need not be asked for.
func (a *Admin) HostsWithLocalConf(ctx context.Context) ([]string, error) {
	return a.q.ShowHostConfigurations()
}

// HostConf reads a host's local configuration (qconf -sconf <host>).
func (a *Admin) HostConf(ctx context.Context, host string) (ClusterConf, error) {
	h, err := a.q.ShowHostConfiguration(host)
	if err != nil {
		return ClusterConf{}, err
	}
	conf := ClusterConf{ExecdParams: strings.Join(dropNone(h.ExecdParams), ",")}
	if h.RshDaemon != nil {
		conf.RshDaemon = *h.RshDaemon
	}
	return conf, nil
}

// SystemdDisabled reports whether execd_params turns systemd off on a host.
// A host's local execd_params replaces the global one as a whole, so the
// global setting counts only when the host sets none of its own (local nil or
// empty).
func SystemdDisabled(local *ClusterConf, global ClusterConf) bool {
	if local != nil && local.ExecdParams != "" {
		disabled, _ := systemdSetting([]string{local.ExecdParams})
		return disabled
	}
	disabled, _ := systemdSetting([]string{global.ExecdParams})
	return disabled
}

// ExecHost is the shim's view of an execution host: its complex_values.
type ExecHost struct {
	Name          string
	ComplexValues map[string]string
}

// ExecHost reads one execution host (qconf -se).
func (a *Admin) ExecHost(ctx context.Context, host string) (ExecHost, error) {
	h, err := a.q.ShowExecHost(host)
	if err != nil {
		return ExecHost{}, err
	}
	return ExecHost{Name: h.Name, ComplexValues: h.ComplexValues}, nil
}

// SlotsLimited reports whether the host sets a slots limit in its
// complex_values -- what keeps two queues on one host from together running
// more jobs than it has cores.
func (h ExecHost) SlotsLimited() bool {
	_, ok := h.ComplexValues["slots"]
	return ok
}

// ResourceMap reads the instances of the RSMAP complex name on the host.
// found is false when the host does not define that complex.
func (h ExecHost) ResourceMap(name string) (instances []ResourceMapInstance, found bool, err error) {
	value, ok := h.ComplexValues[name]
	if !ok {
		return nil, false, nil
	}
	_, parsed, err := qconf.ParseResourceMap(value)
	if err != nil {
		return nil, true, err
	}
	for _, p := range parsed {
		instances = append(instances, ResourceMapInstance{ID: p.ID, Characteristics: p.Characteristics})
	}
	return instances, true, nil
}
