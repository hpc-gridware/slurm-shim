package srun

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

var _ = Describe("gpuAssignment [REQ-GPU-003]", func() {
	It("publishes the per-rank devices under shim isolation (the default)", func() {
		cfg := config.Default() // GPU.Isolation == "shim"
		Expect(gpuAssignment(cfg, []int{0, 1})).To(Equal([]int{0, 1}))
	})

	It("suppresses CUDA_VISIBLE_DEVICES under cgroup isolation", func() {
		cfg := config.Default()
		cfg.GPU.Isolation = "cgroup"
		Expect(gpuAssignment(cfg, []int{0, 1})).To(BeNil())
	})

	It("tolerates a nil config", func() {
		Expect(gpuAssignment(nil, []int{0})).To(Equal([]int{0}))
	})
})
