package fabricator_test

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/fabricator"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func TestFabricator(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Fabricator Suite")
}

// testConfig returns a config with the three standard PEs mapped to policies.
func testConfig() *config.Config {
	c := config.Default()
	c.PEs = map[string]config.PE{
		"mpi.pe": {TaskPolicy: "slot"},
		"smp.pe": {TaskPolicy: "node"},
		"gpu.pe": {TaskPolicy: "gpu"},
	}
	c.PartitionAliases = map[string]string{"all.q": "batch", "gpu.q": "gpu"}
	return c
}

// fab runs the fabricator with a fixture environment and a deterministic
// Identity, writing any PE_HOSTFILE content to a temp file first.
func fab(env map[string]string, hostfile string, cfg *config.Config) (*fabricator.Result, error) {
	m := map[string]string{}
	for k, v := range env {
		m[k] = v
	}
	if hostfile != "" {
		path := filepath.Join(GinkgoT().TempDir(), "pe_hostfile")
		Expect(os.WriteFile(path, []byte(hostfile), 0o600)).To(Succeed())
		m["PE_HOSTFILE"] = path
	}
	id := fake.Identity{
		HostnameVal: "node001",
		UserName:    "alice",
		UID:         1000,
		GID:         1000,
		IPs:         map[string]string{"node001": "10.0.0.11", "node002": "10.0.0.12"},
	}
	return fabricator.Fabricate(fabricator.Options{
		Env:      func(k string) string { return m[k] },
		Config:   cfg,
		Identity: id,
		NowUnix:  1754481600,
	})
}

// exportMap flattens the ordered exports into a lookup.
func exportMap(r *fabricator.Result) map[string]string {
	m := map[string]string{}
	for _, kv := range r.Exports {
		m[kv.Key] = kv.Value
	}
	return m
}
