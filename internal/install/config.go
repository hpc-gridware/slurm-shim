package install

import (
	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// GenerateConfig folds the plan into a site config. Given an existing config it
// MERGES: partitions the site already defines are never rewritten, only
// missing ones are added, and a default_partition already set is kept. Given
// nil it starts from the compiled-in defaults, which already carry the safe
// memory complex and port ranges.
func GenerateConfig(p Plan, existing *config.Config) *config.Config {
	cfg := existing
	if cfg == nil {
		cfg = config.Default()
	}
	if cfg.Partitions == nil {
		cfg.Partitions = map[string]config.Partition{}
	}
	if cfg.PEs == nil {
		cfg.PEs = map[string]config.PE{}
	}
	for _, part := range p.Partitions {
		if _, exists := cfg.Partitions[part.Name]; exists {
			continue
		}
		cfg.Partitions[part.Name] = config.Partition{
			Queue: part.Queue,
			PE:    p.PE.Name,
			Slots: "per-task",
		}
	}
	if _, exists := cfg.PEs[p.PE.Name]; !exists {
		cfg.PEs[p.PE.Name] = config.PE{TaskPolicy: "slot"}
	}
	if cfg.DefaultPartition == "" {
		cfg.DefaultPartition = p.DefaultPartition
	}
	// Only adopt a discovered RSMAP name when the site has not chosen one: the
	// compiled default is "gpu", so a different discovered name is real signal.
	if p.GPUComplex != "" && cfg.GPU.GresComplex == config.Default().GPU.GresComplex {
		cfg.GPU.GresComplex = p.GPUComplex
	}
	return cfg
}
