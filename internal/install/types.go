// Package install turns a running Open Cluster Scheduler into one that runs
// unmodified SLURM scripts: it discovers what the cluster has, plans the
// changes (a dedicated PE, queue wiring, a generated config), and applies them
// idempotently. The plan is data, so the CLI prints it, a test asserts on it,
// and any other front end can render it; Apply never asks a question.
package install

import (
	"context"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// ClusterAdmin is what the installer needs from the scheduler. gedata.Admin is
// the real one; specs use a fake. Every write needs a manager.
type ClusterAdmin interface {
	PEs(ctx context.Context) ([]string, error)
	PE(ctx context.Context, name string) (gedata.PE, error)
	AddPE(ctx context.Context, p gedata.PE) error
	SetPEAttr(ctx context.Context, pe, attr, value string) error
	Queues(ctx context.Context) ([]string, error)
	Queue(ctx context.Context, name string) (gedata.Queue, error)
	SetQueueAttr(ctx context.Context, queue, attr, value string) error
	AddQueueAttr(ctx context.Context, queue, attr, value string) error
	Complexes(ctx context.Context) ([]gedata.Complex, error)
	ExecHosts(ctx context.Context) ([]string, error)
}

// Facts is everything Discover learned. Plan is a pure function of Facts and
// Options, which is what makes it testable without a cluster.
type Facts struct {
	Queues    []gedata.Queue
	PEs       []gedata.PE
	Complexes []gedata.Complex
	ExecHosts []string
}

// Options steer the plan. Zero values are the documented defaults.
type Options struct {
	// Prefix is the runtime tree, e.g. /opt/ocs/slurm-shim. The PE's
	// start_proc_args and the queues' starter_method point into it.
	Prefix string
	// PEName is the dedicated PE to create (default "slurm-shim"). With Force it
	// may name an existing PE, whose start_proc_args is then overwritten.
	PEName string
	// Queues limits wiring to these cluster queues; empty means all of them.
	Queues []string
	// Force allows overwriting an existing PE's start_proc_args and a queue's
	// existing starter_method. Without it both are refused (see Plan).
	Force bool
}

// ChangeKind says what a Change does, so a renderer can group or colour it.
type ChangeKind string

const (
	ChangeAddPE       ChangeKind = "add-pe"
	ChangeSetPEAttr   ChangeKind = "set-pe-attr"
	ChangeAddToPEList ChangeKind = "add-pe-to-queue"
	ChangeSetStarter  ChangeKind = "set-starter"
	ChangeRefused     ChangeKind = "refused"
	ChangeUnchanged   ChangeKind = "unchanged"
)

// Change is one planned mutation, with enough to render and enough to apply.
// Old is "" when the attribute was unset. Reason says why the plan wants it,
// or -- for a refusal -- why it will not do it without Force.
type Change struct {
	Kind   ChangeKind
	Object string // PE or queue name
	Attr   string // GE attribute name, "" for add-pe
	Old    string
	New    string
	Reason string
	// pe carries the whole object for ChangeAddPE.
	pe gedata.PE
}

// Plan is the ordered set of changes plus what the plan decided about the
// site's layout, so the generated config and the printed summary agree.
type Plan struct {
	Changes []Change
	// PE is the parallel environment the partitions will use.
	PE gedata.PE
	// Partitions maps partition name -> queue name, in the order they should
	// appear in config.yaml.
	Partitions []Partition
	// DefaultPartition is the partition name the config will default to.
	DefaultPartition string
	// GPUComplex is the RSMAP complex found, "" when none.
	GPUComplex string
}

// Partition is one queue exposed under a SLURM partition name.
type Partition struct {
	Name  string
	Queue string
}

// Mutating reports whether the plan contains anything that would change the
// cluster (as opposed to only unchanged rows and refusals).
func (p Plan) Mutating() bool {
	for _, c := range p.Changes {
		switch c.Kind {
		case ChangeAddPE, ChangeSetPEAttr, ChangeAddToPEList, ChangeSetStarter:
			return true
		}
	}
	return false
}

// Refusals returns the changes the plan declined to make without Force.
func (p Plan) Refusals() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Kind == ChangeRefused {
			out = append(out, c)
		}
	}
	return out
}
