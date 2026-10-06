package gedata

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// Queue is the shim's view of a cluster queue. PEList and StarterMethod are
// the queue's default entries; per-host overrides ("[@gpu=mpi slurm-shim]")
// are kept apart, because qconf -aattr/-dattr on the queue name touch only
// the default entry.
type Queue struct {
	Name             string
	PEList           []string
	PEListOverrides  []string
	StarterMethod    string // "" when GE prints NONE
	StarterOverrides []string
	ShellStartMode   string
	// Subordinates is the subordinate_list (default entry).
	Subordinates []string
	// MemoryLimits are the per-slot memory rlimits the queue sets, as sorted
	// "name=value" entries; INFINITY (no limit) is left out.
	MemoryLimits []string
}

// Complex is one row of `qconf -sc`.
type Complex struct {
	Name        string
	Shortcut    string
	Type        string // MEMORY, INT, RSMAP, ...
	Consumable  string // YES, NO, JOB, HOST
	Requestable string // YES, NO, FORCED
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
	pes, peOverrides := splitOverrides(dropNone(c.PeList))
	starter, starterOverrides := splitOverrides(c.StarterMethod)
	subs, _ := splitOverrides(dropNone(c.SubordinateList))
	return Queue{
		Name:             c.Name,
		PEList:           pes,
		PEListOverrides:  peOverrides,
		StarterMethod:    firstOrEmpty(starter),
		StarterOverrides: starterOverrides,
		ShellStartMode:   firstOrEmpty(c.ShellStartMode),
		Subordinates:     subs,
		MemoryLimits:     queueMemoryLimits(c),
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

// RemoveQueueAttr removes a value from a list-valued queue attribute (qconf
// -dattr queue), e.g. a PE from pe_list. A value that is not there is not an
// error: removal is idempotent.
func (a *Admin) RemoveQueueAttr(ctx context.Context, queue, attr, value string) error {
	if err := a.q.DeleteAttribute("queue", attr, value, queue); err != nil && !errors.Is(err, qconf.ErrNoModification) {
		return err
	}
	return nil
}

// CloneQueue creates queue name as a copy of src -- hostlist, slots, limits,
// shell settings -- with an empty pe_list and no starter_method, so the new
// queue offers nothing until it is wired. Two things are deliberately not
// copied: src's queue-level complex_values (a consumable there would double its
// capacity on every host) and its subordinate_list (the new queue would suspend
// other queues without anyone asking). complexValues ("slurm_shim=TRUE") become
// the new queue's complex_values. An empty src creates it on @allhosts with the
// scheduler's queue defaults. src itself is never written.
func (a *Admin) CloneQueue(ctx context.Context, src, name, complexValues string) error {
	cfg := qconf.ClusterQueueConfig{HostList: []string{"@allhosts"}}
	if src != "" {
		c, err := a.q.ShowClusterQueue(src)
		if err != nil {
			return err
		}
		cfg = c
	}
	cfg.Name = name
	cfg.PeList = []string{"NONE"}
	cfg.StarterMethod = []string{"NONE"}
	cfg.SubordinateList = []string{"NONE"}
	cfg.ComplexValues = []string{orNONE(complexValues)}
	return a.q.AddClusterQueue(cfg)
}

// AddForcedComplex adds a boolean complex that is requestable FORCED: a queue
// that sets it in complex_values only runs jobs that request it (-l name=TRUE),
// so no job lands there by accident.
func (a *Admin) AddForcedComplex(ctx context.Context, name string) error {
	return a.q.AddComplexEntry(qconf.ComplexEntryConfig{
		Name: name, Shortcut: name, Type: "BOOL", Relop: "==", Requestable: "FORCED",
		Consumable: "NO", Default: "FALSE", Urgency: 0,
	})
}

// DeleteComplex deletes a complex entry. The scheduler refuses while a queue
// or host still references it.
func (a *Admin) DeleteComplex(ctx context.Context, name string) error {
	return a.q.DeleteComplexEntry(name)
}

// RQS is one resource quota set: its name and its limit rules, as qconf -srqs
// prints them ("users {*} queues all.q to slots=10").
type RQS struct {
	Name    string
	Enabled bool
	Limits  []string
}

// ResourceQuotaSets reads every resource quota set.
func (a *Admin) ResourceQuotaSets(ctx context.Context) ([]RQS, error) {
	names, err := a.q.ShowResourceQuotaSets()
	if err != nil {
		return nil, err
	}
	var out []RQS
	for _, n := range dropNone(names) {
		r, err := a.q.ShowResourceQuotaSet(n)
		if err != nil {
			return nil, err
		}
		out = append(out, RQS{Name: r.Name, Enabled: r.Enabled, Limits: r.Limits})
	}
	return out, nil
}

// DeleteQueue deletes a cluster queue (qconf -dq).
func (a *Admin) DeleteQueue(ctx context.Context, name string) error {
	return a.q.DeleteClusterQueue(name)
}

// DeletePE deletes a parallel environment (qconf -dp). The scheduler refuses
// while a job still uses it.
func (a *Admin) DeletePE(ctx context.Context, name string) error {
	return a.q.DeleteParallelEnvironment(name)
}

// HostSlotsLimited reports whether exec host sets a slots limit in its
// complex_values -- what keeps two queues on one host from together running
// more jobs than it has cores.
func (a *Admin) HostSlotsLimited(ctx context.Context, host string) (bool, error) {
	h, err := a.ExecHost(ctx, host)
	if err != nil {
		return false, err
	}
	return h.SlotsLimited(), nil
}

// Complexes lists the complex entries.
func (a *Admin) Complexes(ctx context.Context) ([]Complex, error) {
	rows, err := a.q.ShowAllComplexes()
	if err != nil {
		return nil, err
	}
	out := make([]Complex, 0, len(rows))
	for _, r := range rows {
		out = append(out, Complex{Name: r.Name, Shortcut: r.Shortcut, Type: r.Type, Consumable: r.Consumable, Requestable: r.Requestable})
	}
	return out, nil
}

// ExecHosts lists the execution host names.
func (a *Admin) ExecHosts(ctx context.Context) ([]string, error) {
	return a.q.ShowExecHosts()
}

// SubmitHosts lists the cluster's submit hosts (qconf -ss).
func (a *Admin) SubmitHosts(ctx context.Context) ([]string, error) {
	return a.q.ShowSubmitHosts()
}

// ResourceMapInstance is one instance of a host's RSMAP definition.
type ResourceMapInstance struct {
	ID string
	// Characteristics holds the instance's characteristics block, nil for a
	// bare id. "devices" is what Gridware Cluster Scheduler confines a job to.
	Characteristics map[string]string
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

// firstOrEmpty reads a GE single-valued attribute the library returns as a
// list, mapping GE's NONE to "".
func firstOrEmpty(vs []string) string {
	if len(vs) == 0 || strings.EqualFold(vs[0], "NONE") {
		return ""
	}
	return vs[0]
}

// dropNone removes GE's NONE placeholder from a list attribute.
// RQSLimitsHostSlots reports whether an enabled resource quota limits slots
// per host ("hosts {*} to slots=$num_proc"), which bounds every queue on a host
// together, as an exechost slots limit does.
func RQSLimitsHostSlots(sets []RQS) bool {
	for _, r := range sets {
		if !r.Enabled {
			continue
		}
		for _, l := range r.Limits {
			if slices.Contains(strings.Fields(l), "hosts") && strings.Contains(l, "slots=") {
				return true
			}
		}
	}
	return false
}

// splitOverrides separates a list attribute's default entries from its
// per-host override entries ("[host=...]").
func splitOverrides(vs []string) (defaults, overrides []string) {
	for _, v := range vs {
		if strings.HasPrefix(v, "[") {
			overrides = append(overrides, v)
		} else {
			defaults = append(defaults, v)
		}
	}
	return defaults, overrides
}

// orNONE is v, or GE's literal NONE for an empty value.
func orNONE(v string) string {
	if v == "" {
		return "NONE"
	}
	return v
}

func dropNone(vs []string) []string {
	var out []string
	for _, v := range vs {
		if v != "" && !strings.EqualFold(v, "NONE") {
			out = append(out, v)
		}
	}
	return out
}
