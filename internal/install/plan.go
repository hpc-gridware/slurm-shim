package install

import (
	"path/filepath"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// DefaultPEName is the dedicated PE the installer creates. A dedicated PE,
// rather than modifying one the site has, is the safety decision that matters
// most here: sites use start_proc_args for MPI tight integration, and
// overwriting it silently breaks their MPI.
const DefaultPEName = "slurm-shim"

// ReferencePE is the PE shape the shim needs, verified against the working
// test cluster (qconf -sp make). control_slaves TRUE is what lets srun launch
// steppers with qrsh -inherit; $round_robin spreads slots across nodes so a
// --nodes request can be pinned with -par; the two forks flags stay FALSE
// because the stepper forks its own ranks.
func ReferencePE(name, prefix string) gedata.PE {
	return gedata.PE{
		Name:              name,
		Slots:             999,
		StartProcArgs:     filepath.Join(prefix, "bin", "slurm-shim-env"),
		StopProcArgs:      "NONE",
		AllocationRule:    "$round_robin",
		ControlSlaves:     true,
		JobIsFirstTask:    false,
		MasterForksSlaves: false,
		DaemonForksSlaves: false,
	}
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
	targets := f.Queues
	if len(o.Queues) > 0 {
		targets = nil
		wantQ := map[string]bool{}
		for _, q := range o.Queues {
			wantQ[q] = true
		}
		for _, q := range f.Queues {
			if wantQ[q.Name] {
				targets = append(targets, q)
			}
		}
	}
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
		p.Partitions = append(p.Partitions, Partition{Name: partitionName(q.Name), Queue: q.Name})
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
	type fix struct{ attr, old, new string }
	var fixes []fix
	if have.StartProcArgs != want.StartProcArgs {
		fixes = append(fixes, fix{"start_proc_args", have.StartProcArgs, want.StartProcArgs})
	}
	if !have.ControlSlaves {
		fixes = append(fixes, fix{"control_slaves", "FALSE", "TRUE"})
	}
	if len(fixes) == 0 {
		return []Change{{Kind: ChangeUnchanged, Object: have.Name, Attr: "start_proc_args", Old: have.StartProcArgs, New: have.StartProcArgs}}
	}
	var out []Change
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
