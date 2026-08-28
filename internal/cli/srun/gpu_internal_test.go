package srun

import (
	"io"

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

var _ = Describe("per-task binding under cgroup isolation [REQ-GPU-003]", func() {
	// gpuAssignment returns nil under cgroup isolation regardless of what was
	// asked for, so a per-task binding request is accepted and then dropped.
	// These pin that it is reported, and -- just as important -- that it is NOT
	// reported when no binding was actually requested. A warning that fires on
	// every CPU-only step would train users to ignore it.
	cgroupCfg := func() *config.Config {
		cfg := config.Default()
		cfg.GPU.Isolation = "cgroup"
		return cfg
	}
	// resolve runs the real pair, in the order run.go calls them.
	resolve := func(opt *options, cfg *config.Config, envBind string) {
		applyGPUBind(opt, cfg, envBind)
		warnCgroupCannotBind(opt, cfg)
	}
	// wantCgroupWarning asserts the cgroup warning specifically. A bare length
	// check would also pass on the "--gpu-bind=... is not supported" warning the
	// same code path can emit, which is a different bug entirely.
	wantCgroupWarning := func(opt *options) {
		ExpectWithOffset(1, opt.warnings).To(HaveLen(1))
		ExpectWithOffset(1, opt.warnings[0]).To(ContainSubstring(`gpu.isolation is "cgroup"`))
		ExpectWithOffset(1, opt.warnings[0]).To(ContainSubstring("gpu.isolation: shim"))
		ExpectWithOffset(1, opt.warnings[0]).NotTo(ContainSubstring("not supported"))
	}

	It("warns when --gpus-per-task cannot be honored", func() {
		opt := &options{}
		opt.req.GPUsPerTask = 1
		resolve(opt, cgroupCfg(), "")
		wantCgroupWarning(opt)
	})

	It("warns when --gpu-bind=per_task cannot be honored", func() {
		opt := &options{gpuBind: "per_task"}
		resolve(opt, cgroupCfg(), "")
		wantCgroupWarning(opt)
		Expect(opt.req.AutoDivideGPUs).To(BeTrue(), "the request must be recognized, not just warned about")
	})

	It("warns when the binding request arrives via SLURM_GPU_BIND", func() {
		opt := &options{}
		resolve(opt, cgroupCfg(), "per_task:1")
		wantCgroupWarning(opt)
		Expect(opt.req.AutoDivideGPUs).To(BeTrue())
	})

	It("reaches the field from the actual --gpus-per-task flag, not just a hand-set struct", func() {
		opt, err := parseFlags([]string{"--gpus-per-task=1", "true"}, false, io.Discard)
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.req.GPUsPerTask).To(Equal(1))
		warnCgroupCannotBind(opt, cgroupCfg())
		wantCgroupWarning(opt)
	})

	It("stays quiet on a CPU-only step at a site whose default is per-task", func() {
		// The site default alone must not warn: this runs before placement, so it
		// cannot tell a GPU step from a CPU one, and firing here would put a GPU
		// warning on every single step at such a site.
		cfg := cgroupCfg()
		cfg.GPU.Bind = "per-task"
		opt := &options{}
		resolve(opt, cfg, "")
		Expect(opt.warnings).To(BeEmpty())
	})

	It("stays quiet when --gpu-bind=none explicitly declines binding", func() {
		opt := &options{gpuBind: "none"}
		opt.req.GPUsPerTask = 1
		resolve(opt, cgroupCfg(), "")
		Expect(opt.warnings).To(BeEmpty())
		Expect(opt.req.GPUsPerTask).To(Equal(0), "an explicit none must clear the per-task request")
	})

	It("stays quiet when no per-task binding was asked for", func() {
		opt := &options{}
		resolve(opt, cgroupCfg(), "")
		Expect(opt.warnings).To(BeEmpty())
	})

	It("stays quiet under shim isolation, where the binding does happen", func() {
		opt := &options{}
		opt.req.GPUsPerTask = 1
		resolve(opt, config.Default(), "")
		Expect(opt.warnings).To(BeEmpty())
	})
})
