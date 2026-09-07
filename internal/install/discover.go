package install

import (
	"context"
	"fmt"
	"sort"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// Discover reads the cluster's queues, PEs, complexes and exec hosts. It is
// read-only and needs no manager.
func Discover(ctx context.Context, a ClusterAdmin) (Facts, error) {
	var f Facts

	qnames, err := a.Queues(ctx)
	if err != nil {
		return f, fmt.Errorf("listing queues: %w", err)
	}
	sort.Strings(qnames)
	for _, n := range qnames {
		q, err := a.Queue(ctx, n)
		if err != nil {
			return f, fmt.Errorf("reading queue %s: %w", n, err)
		}
		f.Queues = append(f.Queues, q)
	}

	penames, err := a.PEs(ctx)
	if err != nil {
		return f, fmt.Errorf("listing parallel environments: %w", err)
	}
	sort.Strings(penames)
	for _, n := range penames {
		p, err := a.PE(ctx, n)
		if err != nil {
			return f, fmt.Errorf("reading pe %s: %w", n, err)
		}
		f.PEs = append(f.PEs, p)
	}

	if f.Complexes, err = a.Complexes(ctx); err != nil {
		return f, fmt.Errorf("listing complexes: %w", err)
	}
	if f.ExecHosts, err = a.ExecHosts(ctx); err != nil {
		return f, fmt.Errorf("listing exec hosts: %w", err)
	}
	sort.Strings(f.ExecHosts)
	return f, nil
}

// findPE returns the PE with the given name, if present.
func (f Facts) findPE(name string) (gedata.PE, bool) {
	for _, p := range f.PEs {
		if p.Name == name {
			return p, true
		}
	}
	return gedata.PE{}, false
}
