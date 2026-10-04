package install

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// DefaultPEName is the dedicated PE the installer creates. A dedicated PE,
// rather than modifying one the site has, is the safety decision that matters
// most here: sites use start_proc_args for MPI tight integration, and
// overwriting it silently breaks their MPI.
const DefaultPEName = "slurm-shim"

// PESlotsNoCap is the dedicated PE's slots: the sge_pe(5) maximum, i.e. no
// PE-level cap. A PE's slots limit every job running in it at once, together,
// so a value sized to today's cluster goes stale the day nodes are added; queue
// and host slots still bound what runs. A site that wants a cap sets one.
const PESlotsNoCap = 9999999

// legacyPESlots is what earlier installers gave the dedicated PE. An install
// still at exactly this value is raised to PESlotsNoCap: it is the installer's
// own old default, not a site's choice.
const legacyPESlots = 999

// ReferencePE is the PE shape the shim needs, verified against the working
// test cluster (qconf -sp make). control_slaves TRUE is what lets srun launch
// steppers with qrsh -inherit; $round_robin spreads slots across nodes so a
// --nodes request can be pinned with -par; the two forks flags stay FALSE
// because the stepper forks its own ranks.
func ReferencePE(name, prefix string) gedata.PE {
	return gedata.PE{
		Name:              name,
		Slots:             PESlotsNoCap,
		StartProcArgs:     filepath.Join(prefix, "bin", "slurm-shim-env"),
		StopProcArgs:      "NONE",
		AllocationRule:    "$round_robin",
		ControlSlaves:     true,
		JobIsFirstTask:    false,
		MasterForksSlaves: false,
		DaemonForksSlaves: false,
	}
}

// DefaultQueueName is the queue a first install creates when no --queue is
// given, so evaluating the shim changes no existing queue.
const DefaultQueueName = "slurm.q"

// ShimComplex is the FORCED boolean complex DefaultQueueName carries: a job
// enters the queue only by requesting it, so site jobs that do not ask for the
// shim never land there. ShimRequest is that request, added by sbatch and srun
// for the partition (config partitions.<name>.request).
const (
	ShimComplex = "slurm_shim"
	ShimRequest = ShimComplex + "=TRUE"
)

// AllQueues is the --queue value that wires every cluster queue.
const AllQueues = "all"

// AmbiguousAllQueues reports whether --queue all could also mean a queue that
// is literally named "all"; the installer then refuses to guess.
func AmbiguousAllQueues(f Facts, queues []string) bool {
	_, named := f.findQueue(AllQueues)
	return named && len(queues) == 1 && queues[0] == AllQueues
}

// selectQueues decides which queues to wire. "all" wires every queue and named
// queues are taken as given (an unknown name is refused, not skipped). Without
// --queue, the queues already wired to this tree are kept -- a re-run changes
// nothing -- and a first install creates DefaultQueueName as a clone of all.q,
// entered only by jobs that request ShimComplex. A DefaultQueueName this
// install did not create is refused, never adopted.
func (p *Plan) selectQueues(f Facts, o Options, starter string) []gedata.Queue {
	switch {
	case len(o.Queues) == 1 && o.Queues[0] == AllQueues:
		return f.Queues
	case contains(o.Queues, AllQueues):
		p.Changes = append(p.Changes, Change{
			Kind: ChangeRefused, Object: AllQueues, Attr: "queue",
			Reason: "--queue all wires every queue and cannot be combined with named queues",
		})
		return nil
	case len(o.Queues) > 0:
		var targets []gedata.Queue
		for _, name := range o.Queues {
			q, ok := f.findQueue(name)
			if !ok {
				p.Changes = append(p.Changes, Change{
					Kind: ChangeRefused, Object: name, Attr: "queue",
					Reason: "no cluster queue of this name (qconf -sql lists them)",
				})
				continue
			}
			targets = append(targets, q)
		}
		return targets
	}
	var wired []gedata.Queue
	for _, q := range f.Queues {
		if q.StarterMethod == starter {
			wired = append(wired, q)
		}
	}
	if len(wired) > 0 {
		return wired
	}
	if q, exists := f.findQueue(DefaultQueueName); exists {
		if o.CreatedQueue == DefaultQueueName {
			return []gedata.Queue{q} // an interrupted first install: finish wiring it
		}
		p.Changes = append(p.Changes, Change{
			Kind: ChangeRefused, Object: DefaultQueueName, Attr: "queue",
			Reason: "a queue " + DefaultQueueName + " exists and is not wired to this install; " +
				"name the queues to wire with --queue (or --queue all)",
		})
		return nil
	}
	if c, exists := f.findComplex(ShimComplex); !exists {
		p.Changes = append(p.Changes, Change{
			Kind: ChangeAddComplex, Object: ShimComplex,
			Reason: "BOOL, requestable FORCED: only jobs that request it enter " + DefaultQueueName,
		})
	} else if !strings.EqualFold(c.Type, "BOOL") || !strings.EqualFold(c.Requestable, "FORCED") {
		p.Changes = append(p.Changes, Change{
			Kind: ChangeRefused, Object: ShimComplex, Attr: "complex",
			Old: c.Type + " requestable " + c.Requestable, New: "BOOL requestable FORCED",
			Reason: "a complex " + ShimComplex + " exists with another meaning; " +
				"name the queues to wire with --queue (or --queue all)",
		})
		return nil
	}
	src := ""
	if _, ok := f.findQueue("all.q"); ok {
		src = "all.q"
	}
	p.Changes = append(p.Changes, Change{
		Kind: ChangeAddQueue, Object: DefaultQueueName, Old: src, New: ShimRequest,
		Reason: "a queue of its own that only shim jobs enter, so no existing queue changes " +
			"(--queue all wires every queue)",
	})
	return []gedata.Queue{{Name: DefaultQueueName}}
}

// partitionRequest is the -l request a partition on queue needs: ShimRequest
// for the shim's own queue (while the complex exists or is being added).
func (p *Plan) partitionRequest(f Facts, queue string) string {
	if queue != DefaultQueueName {
		return ""
	}
	if _, ok := f.findComplex(ShimComplex); ok {
		return ShimRequest
	}
	for _, c := range p.Changes {
		if c.Kind == ChangeAddComplex {
			return ShimRequest
		}
	}
	return ""
}

// StarterPath is the queue starter_method for a prefix.
func StarterPath(prefix string) string {
	return filepath.Join(prefix, "bin", "slurm-shim-starter")
}

// MakePlan decides what to change. It is a pure function: no I/O, so every
// branch is a unit spec.
func MakePlan(f Facts, o Options) Plan {
	if o.PEName == "" {
		o.PEName = DefaultPEName
	}
	want := ReferencePE(o.PEName, o.Prefix)
	starter := StarterPath(o.Prefix)
	var p Plan
	p.PE = want

	// The PE: create, accept, refuse, or (forced) repair.
	if have, ok := f.findPE(o.PEName); !ok {
		p.Changes = append(p.Changes, Change{
			Kind: ChangeAddPE, Object: o.PEName, New: want.StartProcArgs,
			Reason: "dedicated PE with control_slaves TRUE and the shim's start_proc_args", pe: want,
		})
	} else {
		p.Changes = append(p.Changes, planPERepair(have, want, o.Force)...)
	}

	// The queues.
	targets := p.selectQueues(f, o, starter)
	for _, q := range targets {
		if !contains(q.PEList, o.PEName) {
			p.Changes = append(p.Changes, Change{
				Kind: ChangeAddToPEList, Object: q.Name, Attr: "pe_list",
				Old: strings.Join(q.PEList, " "), New: o.PEName,
				Reason: "queue must offer the PE for -pe " + o.PEName + " to place jobs",
			})
		} else {
			p.Changes = append(p.Changes, Change{Kind: ChangeUnchanged, Object: q.Name, Attr: "pe_list", Old: o.PEName, New: o.PEName})
		}
		switch {
		case q.StarterMethod == starter:
			p.Changes = append(p.Changes, Change{Kind: ChangeUnchanged, Object: q.Name, Attr: "starter_method", Old: starter, New: starter})
		case q.StarterMethod == "":
			p.Changes = append(p.Changes, Change{
				Kind: ChangeSetStarter, Object: q.Name, Attr: "starter_method", New: starter,
				Reason: "injects the SLURM_* environment into every job in the queue (REQ-FAB-010)",
			})
		case o.Force:
			p.Changes = append(p.Changes, Change{
				Kind: ChangeSetStarter, Object: q.Name, Attr: "starter_method", Old: q.StarterMethod, New: starter,
				Reason: "replacing the site's starter_method because --force was given",
			})
		default:
			p.Changes = append(p.Changes, Change{
				Kind: ChangeRefused, Object: q.Name, Attr: "starter_method", Old: q.StarterMethod, New: starter,
				Reason: "queue already has a starter_method; two starters cannot be chained. " +
					"Re-run with --force to replace it, or wire this queue by hand",
			})
		}
		p.Partitions = append(p.Partitions, Partition{Name: partitionName(q.Name), Queue: q.Name, Request: p.partitionRequest(f, q.Name)})
	}
	p.DefaultPartition = defaultPartition(p.Partitions)

	for _, c := range f.Complexes {
		if strings.EqualFold(c.Type, "RSMAP") {
			p.GPUComplex = c.Name
			break
		}
	}
	return p
}

// planPERepair compares an existing PE with the reference and either accepts
// it, refuses to touch it, or (with Force) lists the attribute repairs.
func planPERepair(have, want gedata.PE, force bool) []Change {
	var raise []Change
	if have.Name == DefaultPEName && have.Slots == legacyPESlots {
		raise = append(raise, Change{
			Kind: ChangeSetPEAttr, Object: have.Name, Attr: "slots",
			Old: strconv.Itoa(legacyPESlots), New: strconv.Itoa(PESlotsNoCap),
			Reason: "earlier installers capped every job in this PE, together, at 999 slots",
		})
	}
	type fix struct{ attr, old, new string }
	var fixes []fix
	if have.StartProcArgs != want.StartProcArgs {
		fixes = append(fixes, fix{"start_proc_args", have.StartProcArgs, want.StartProcArgs})
	}
	if !have.ControlSlaves {
		fixes = append(fixes, fix{"control_slaves", "FALSE", "TRUE"})
	}
	if len(fixes) == 0 {
		if len(raise) > 0 {
			return raise
		}
		return []Change{{Kind: ChangeUnchanged, Object: have.Name, Attr: "start_proc_args", Old: have.StartProcArgs, New: have.StartProcArgs}}
	}
	out := raise
	for _, x := range fixes {
		if !force {
			out = append(out, Change{
				Kind: ChangeRefused, Object: have.Name, Attr: x.attr, Old: x.old, New: x.new,
				Reason: "PE " + have.Name + " exists with its own " + x.attr + "; sites use start_proc_args " +
					"for MPI tight integration, so it is never overwritten without --force. Prefer a " +
					"dedicated PE (the default) or re-run with --force",
			})
			continue
		}
		out = append(out, Change{
			Kind: ChangeSetPEAttr, Object: have.Name, Attr: x.attr, Old: x.old, New: x.new,
			Reason: "repairing the existing PE because --force was given",
		})
	}
	return out
}

// partitionName is the SLURM-facing name for a queue: all.q -> all.
func partitionName(queue string) string {
	return strings.TrimSuffix(queue, ".q")
}

// defaultPartition picks the partition a job lands in with no -p: all.q's if
// the site has one, else the only one, else the first alphabetically. Sites
// with an opinion set default_partition themselves.
func defaultPartition(ps []Partition) string {
	if len(ps) == 0 {
		return ""
	}
	for _, p := range ps {
		if p.Queue == "all.q" {
			return p.Name
		}
	}
	return ps[0].Name
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
