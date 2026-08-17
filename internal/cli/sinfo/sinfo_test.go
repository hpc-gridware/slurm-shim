package sinfo

import (
	"bytes"
	"io"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

func TestSinfo(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Sinfo Suite")
}

var _ = Describe("sinfo [REQ-SIN-001]", func() {
	It("prints a partition table derived from config, sorted", func() {
		cfg := config.Default()
		cfg.Partitions = map[string]config.Partition{
			"gpu":   {Queue: "gpu.q", PE: "gpu.pe"},
			"batch": {Queue: "all.q", PE: "smp.pe"},
		}
		var out bytes.Buffer
		Expect(run(cfg, nil, &out, io.Discard)).To(Equal(0))
		lines := out.String()
		Expect(lines).To(HavePrefix("PARTITION AVAIL TIMELIMIT NODES STATE NODELIST\n"))
		// Sorted: batch before gpu.
		Expect(lines).To(ContainSubstring("batch up infinite"))
		Expect(lines).To(ContainSubstring("gpu up infinite"))
		Expect(bytes_IndexBatchBeforeGpu(out.Bytes())).To(BeTrue())
	})

	It("prints just the header when no partitions are configured", func() {
		var out bytes.Buffer
		Expect(run(config.Default(), nil, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(Equal("PARTITION AVAIL TIMELIMIT NODES STATE NODELIST\n"))
	})

	It("loads config and lists its partitions end-to-end [REQ-SIN-001]", func() {
		dir := GinkgoT().TempDir()
		cfgPath := dir + "/config.yaml"
		Expect(os.WriteFile(cfgPath, []byte("partitions:\n  gpu: {queue: gpu.q, pe: gpu.pe}\n"), 0o600)).To(Succeed())
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfgPath)

		var out bytes.Buffer
		Expect(Run(nil, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("gpu up infinite"))
	})
})

func bytes_IndexBatchBeforeGpu(b []byte) bool {
	return bytes.Index(b, []byte("batch")) < bytes.Index(b, []byte("gpu"))
}
