package gedata

import (
	"context"
	"fmt"
	"strings"
	"time"

	// go-clusterscheduler's qconf package runs the qconf executable itself, so
	// it cannot take the shim's Runner. Admin therefore lives here, in the one
	// package allowed to exec (REQ-IMP-001), and exposes the shim-owned types
	// below rather than the library's -- the same boundary rule queues.go and
	// accounting.go follow.
	qconf "github.com/hpc-gridware/go-clusterscheduler/pkg/qconf/core"
)

// PE is the shim's view of a parallel environment: the fields the installer
// and doctor reason about, nothing else.
type PE struct {
	Name              string
	Slots             int
	StartProcArgs     string
	StopProcArgs      string
	AllocationRule    string
	ControlSlaves     bool
	JobIsFirstTask    bool
	MasterForksSlaves bool
	DaemonForksSlaves bool
}

// Queue is the shim's view of a cluster queue.
type Queue struct {
	Name           string
	PEList         []string
	StarterMethod  string // "" when GE prints NONE
	ShellStartMode string
}

// Complex is one row of `qconf -sc`.
type Complex struct {
	Name       string
	Shortcut   string
	Type       string // MEMORY, INT, RSMAP, ...
	Consumable string // YES, NO, JOB, HOST
}

// Admin performs the qconf reads and writes the installer and doctor need.
//
// Reads go through the library's parsers into structs. Writes are
// attribute-level (`qconf -mattr`/`-aattr`) wherever the object already exists,
// because a whole-object rewrite (`-Mq`) would drop any attribute the parser
// does not know; only creating a PE writes a whole object, where nothing can be
// lost. Every write needs a manager.
type Admin struct {
	q *qconf.CommandLineQConf
}

// NewAdmin resolves qconf the way every other GE client is resolved
// ($SGE_ROOT/bin/$ARC, then PATH) and returns an Admin over it.
func NewAdmin() (*Admin, error) {
	q, err := qconf.NewCommandLineQConf(qconf.CommandLineQConfConfig{
		Executable: ResolveCommand("qconf"),
		Timeout:    30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("gedata: qconf: %w", err)
	}
	return &Admin{q: q}, nil
}

// PEs lists parallel environment names.
func (a *Admin) PEs(ctx context.Context) ([]string, error) {
	return a.q.ShowParallelEnvironments()
}

// PE reads one parallel environment.
func (a *Admin) PE(ctx context.Context, name string) (PE, error) {
	c, err := a.q.ShowParallelEnvironment(name)
	if err != nil {
		return PE{}, err
	}
	return PE{
		Name:              c.Name,
		Slots:             c.Slots,
		StartProcArgs:     c.StartProcArgs,
		StopProcArgs:      c.StopProcArgs,
		AllocationRule:    c.AllocationRule,
		ControlSlaves:     strings.EqualFold(c.ControlSlaves, "TRUE"),
		JobIsFirstTask:    c.JobIsFirstTask,
		MasterForksSlaves: c.MasterForksSlaves,
		DaemonForksSlaves: c.DaemonForksSlaves,
	}, nil
}

// AddPE creates a parallel environment. The library fills GE's defaults for
// the fields PE does not carry (urgency_slots, user lists).
func (a *Admin) AddPE(ctx context.Context, p PE) error {
	cs := "FALSE"
	if p.ControlSlaves {
		cs = "TRUE"
	}
	return a.q.AddParallelEnvironment(qconf.ParallelEnvironmentConfig{
		Name:              p.Name,
		Slots:             p.Slots,
		StartProcArgs:     p.StartProcArgs,
		StopProcArgs:      p.StopProcArgs,
		AllocationRule:    p.AllocationRule,
		ControlSlaves:     cs,
		JobIsFirstTask:    p.JobIsFirstTask,
		MasterForksSlaves: p.MasterForksSlaves,
		DaemonForksSlaves: p.DaemonForksSlaves,
	})
}

// SetPEAttr sets one attribute of an existing PE (qconf -mattr pe).
func (a *Admin) SetPEAttr(ctx context.Context, pe, attr, value string) error {
	return a.q.ModifyAttribute("pe", attr, value, pe)
}

// Queues lists cluster queue names.
func (a *Admin) Queues(ctx context.Context) ([]string, error) {
	return a.q.ShowClusterQueues()
}

// Queue reads one cluster queue.
func (a *Admin) Queue(ctx context.Context, name string) (Queue, error) {
	c, err := a.q.ShowClusterQueue(name)
	if err != nil {
		return Queue{}, err
	}
	return Queue{
		Name:           c.Name,
		PEList:         dropNone(c.PeList),
		StarterMethod:  firstOrEmpty(c.StarterMethod),
		ShellStartMode: firstOrEmpty(c.ShellStartMode),
	}, nil
}

// SetQueueAttr sets one queue attribute (qconf -mattr queue).
func (a *Admin) SetQueueAttr(ctx context.Context, queue, attr, value string) error {
	return a.q.ModifyAttribute("queue", attr, value, queue)
}

// AddQueueAttr adds a value to a list-valued queue attribute (qconf -aattr
// queue), e.g. a PE to pe_list, leaving the existing entries alone.
func (a *Admin) AddQueueAttr(ctx context.Context, queue, attr, value string) error {
	return a.q.AddAttribute("queue", attr, value, queue)
}

// Complexes lists the complex entries.
func (a *Admin) Complexes(ctx context.Context) ([]Complex, error) {
	rows, err := a.q.ShowAllComplexes()
	if err != nil {
		return nil, err
	}
	out := make([]Complex, 0, len(rows))
	for _, r := range rows {
		out = append(out, Complex{Name: r.Name, Shortcut: r.Shortcut, Type: r.Type, Consumable: r.Consumable})
	}
	return out, nil
}

// ExecHosts lists the execution host names.
func (a *Admin) ExecHosts(ctx context.Context) ([]string, error) {
	return a.q.ShowExecHosts()
}

// ResourceMapInstance is one instance of a host's RSMAP definition.
type ResourceMapInstance struct {
	ID string
	// Characteristics holds the instance's characteristics block, nil for a
	// bare id. "devices" is what Gridware Cluster Scheduler confines a job to.
	Characteristics map[string]string
}

// ExecHostResourceMap reads the instances of the RSMAP complex name on host.
// found is false when the host does not define that complex.
func (a *Admin) ExecHostResourceMap(ctx context.Context, host, name string) (instances []ResourceMapInstance, found bool, err error) {
	h, err := a.q.ShowExecHost(host)
	if err != nil {
		return nil, false, err
	}
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

// SystemdDisabled reports whether execd_params turns systemd off on host.
// A host's local execd_params replaces the global one as a whole, so the
// global setting is consulted only when the host has none of its own.
func (a *Admin) SystemdDisabled(ctx context.Context, host string) (bool, error) {
	// An error here means the host has no local configuration, which is normal.
	if local, err := a.q.ShowHostConfiguration(host); err == nil && hasParams(local.ExecdParams) {
		disabled, _ := systemdSetting(local.ExecdParams)
		return disabled, nil
	}
	global, err := a.q.ShowGlobalConfiguration()
	if err != nil {
		return false, err
	}
	disabled, _ := systemdSetting(global.ExecdParams)
	return disabled, nil
}

// systemdSetting reads ENABLE_SYSTEMD from execd_params. set is false when the
// parameter is absent, in which case systemd is on (the default). Entries may
// hold several comma- or space-separated parameters: the library splits the
// global configuration but keeps a host configuration's value whole.
func systemdSetting(params []string) (disabled, set bool) {
	for _, p := range params {
		for _, token := range strings.FieldsFunc(p, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			key, value, ok := strings.Cut(token, "=")
			if ok && strings.EqualFold(key, "ENABLE_SYSTEMD") {
				return strings.EqualFold(value, "FALSE") || value == "0", true
			}
		}
	}
	return false, false
}

// hasParams reports whether an execd_params list carries any value other
// than GE's NONE placeholder.
func hasParams(params []string) bool {
	return len(dropNone(params)) > 0
}

// firstOrEmpty reads a GE single-valued attribute the library returns as a
// list, mapping GE's NONE to "".
func firstOrEmpty(vs []string) string {
	if len(vs) == 0 || strings.EqualFold(vs[0], "NONE") {
		return ""
	}
	return vs[0]
}

// dropNone removes GE's NONE placeholder from a list attribute.
func dropNone(vs []string) []string {
	var out []string
	for _, v := range vs {
		if v != "" && !strings.EqualFold(v, "NONE") {
			out = append(out, v)
		}
	}
	return out
}
