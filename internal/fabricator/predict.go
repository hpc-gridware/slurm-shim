package fabricator

import (
	"context"
	"errors"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// PredictedNode is one node of a hypothetical allocation. Slots and GPUs are the
// grant the caller is modelling; Name is a placeholder, since the real host names
// come from PE_HOSTFILE at run time.
type PredictedNode struct {
	Name  string
	Slots int
	GPUs  int
}

// Predict fabricates the environment a job WOULD get from a hypothetical
// allocation, for the submit-time dry run (SLURM_SHIM_DRY_RUN).
//
// It runs the same geometry, layout and Table A assembly as a real job, so the
// reported variables cannot drift from the ones the PE hook will export. Only the
// allocation is supplied by the caller rather than read from PE_HOSTFILE.
//
// opts.Env MUST resolve the job-scoped variables the real fabricator reads --
// including the SLURM_SHIM_* overrides, which qsub -V forwards into the job and
// which change the task geometry. A caller that passes a closed map covering only
// the submit-time facts will silently mispredict exactly those cases.
//
// opts.Runner is ignored: Predict performs no cluster I/O, so a grant-dependent
// value the fabricator would discover (SLURM_MEM_PER_NODE) is absent rather than
// guessed, and the caller is expected to report it as resolved at run time.
func Predict(opts Options, nodes []PredictedNode) (*Result, error) {
	if len(nodes) == 0 {
		return nil, errors.New("predict: at least one node is required")
	}
	cfg := opts.Config
	if cfg == nil {
		cfg = config.Default()
	}
	opts.Config = cfg
	opts.Runner = nil

	e := envReader{get: defaultEnv(opts.Env)}

	res := &Result{Unset: unsetPreamble()}
	// Same short circuit as Fabricate (REQ-FAB-012): under SLURM_SHIM_DISABLE the
	// job receives no SLURM_* variables at all, and a prediction that listed a full
	// Table A for such a job would be exactly backwards.
	if e.get("SLURM_SHIM_DISABLE") != "" {
		res.Disabled = true
		return res, nil
	}

	ns := nodeSet{peName: e.get("PE"), hosts: make([]nodeInfo, len(nodes))}
	for i, n := range nodes {
		// Device ids are positional stand-ins: the count drives the gpu task policy
		// and SLURM_GPUS_ON_NODE, while the real ids come from the RSMAP grant.
		gpus := make([]int, n.GPUs)
		for g := range gpus {
			gpus[g] = g
		}
		ns.hosts[i] = nodeInfo{
			Host: gedata.Host{Name: n.Name, FQDN: n.Name, Slots: n.Slots},
			gpus: gpus,
		}
	}

	if err := assemble(context.Background(), res, e, cfg, opts, ns); err != nil {
		return nil, err
	}
	return res, nil
}
