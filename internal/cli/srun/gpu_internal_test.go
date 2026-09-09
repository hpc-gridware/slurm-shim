package srun

import (
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

var _ = Describe("gpuAssignment [REQ-GPU-003]", func() {
	It("publishes the per-rank devices under shim isolation (the default)", func() {
		cfg := config.Default() // GPU.Isolation == "shim"
		Expect(gpuAssignment(cfg, []string{"0", "1"})).To(Equal([]string{"0", "1"}))
	})

	It("suppresses the device variable under cgroup isolation", func() {
		cfg := config.Default()
		cfg.GPU.Isolation = "cgroup"
		Expect(gpuAssignment(cfg, []string{"0", "1"})).To(BeNil())
	})

	It("tolerates a nil config", func() {
		Expect(gpuAssignment(nil, []string{"0"})).To(Equal([]string{"0"}))
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

var _ = Describe("device variable selection [REQ-GPU-004]", func() {
	It("writes CUDA_VISIBLE_DEVICES by default and drops every device variable", func() {
		// Ours is dropped too. The per-rank overlay puts it back for a rank that
		// holds a device and adds nothing to a rank that does not, so leaving it
		// would let a device-less rank inherit a mask naming devices the job was
		// never granted.
		write, drop, err := gpuEnvVar(config.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(write).To(Equal(proto.EnvCUDADevices))
		Expect(drop).To(ConsistOf(proto.EnvCUDADevices, proto.EnvROCRDevices,
			proto.EnvHIPDevices, proto.EnvGPUDeviceOrdinal))
	})

	It("treats an empty vendor as nvidia", func() {
		cfg := config.Default()
		cfg.GPU.Vendor = ""
		write, _, err := gpuEnvVar(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(write).To(Equal(proto.EnvCUDADevices))
	})

	It("tolerates a nil config", func() {
		write, _, err := gpuEnvVar(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(write).To(Equal(proto.EnvCUDADevices))
	})

	It("writes ROCR_VISIBLE_DEVICES on amd and drops every HIP-level mask", func() {
		cfg := config.Default()
		cfg.GPU.Vendor = config.VendorAMD
		write, drop, err := gpuEnvVar(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(write).To(Equal(proto.EnvROCRDevices))
		// CUDA and HIP are masks into the already filtered ROCr list, and
		// GPU_DEVICE_ORDINAL is the OpenCL-level equivalent. ROCr itself is in the
		// list too, so a device-less rank cannot keep an inherited one.
		Expect(drop).To(ConsistOf(proto.EnvCUDADevices, proto.EnvROCRDevices,
			proto.EnvHIPDevices, proto.EnvGPUDeviceOrdinal))
	})

	It("refuses an unknown vendor instead of falling back to nvidia", func() {
		cfg := config.Default()
		cfg.GPU.Vendor = "intel"
		_, _, err := gpuEnvVar(cfg)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("intel"))
	})
})

var _ = Describe("inherited device variables are removed, not shadowed [REQ-GPU-004]", func() {
	// The rank overlay only appends, and dedupEnv shadows a key only when the
	// overlay sets it. So a device variable inherited from the job environment --
	// a site prolog, a module file, a container image -- survives unless it is
	// removed outright. On ROCm that inherited value layers on top of the mask the
	// shim writes and silently selects the wrong devices.
	supWith := func(vendor string, gpus []string) *supervisor {
		cfg := config.Default()
		cfg.GPU.Vendor = vendor
		return &supervisor{
			cfg:  cfg,
			plan: &plan.StepPlan{Ranks: []plan.PlacedRank{{GPUs: gpus}}},
		}
	}
	// resolved mirrors what srun does at dispatch: the device variables are
	// resolved once and stored, so nothing later re-derives them.
	resolved := func(vendor string, gpus []string) *supervisor {
		s := supWith(vendor, gpus)
		Expect(s.resolveDeviceVars()).To(Succeed())
		return s
	}
	inherited := []string{
		"HOME=/h",
		"CUDA_VISIBLE_DEVICES=4,5",
		"HIP_VISIBLE_DEVICES=4,5",
		"GPU_DEVICE_ORDINAL=4,5",
		"ROCR_VISIBLE_DEVICES=4,5",
	}

	It("removes every inherited device mask on an amd site", func() {
		got := resolved(config.VendorAMD, []string{"0", "1"}).dropForeignDeviceVars(inherited)
		Expect(got).To(ContainElement("HOME=/h"))
		for _, kv := range got {
			Expect(kv).NotTo(HavePrefix("CUDA_VISIBLE_DEVICES="))
			Expect(kv).NotTo(HavePrefix("ROCR_VISIBLE_DEVICES="))
			Expect(kv).NotTo(HavePrefix("HIP_VISIBLE_DEVICES="))
			Expect(kv).NotTo(HavePrefix("GPU_DEVICE_ORDINAL="))
		}
	})

	It("removes every inherited device mask on an nvidia site too", func() {
		got := resolved(config.VendorNVIDIA, []string{"0", "1"}).dropForeignDeviceVars(inherited)
		Expect(got).To(ContainElement("HOME=/h"))
		for _, kv := range got {
			Expect(kv).NotTo(HavePrefix("CUDA_VISIBLE_DEVICES="))
			Expect(kv).NotTo(HavePrefix("ROCR_VISIBLE_DEVICES="))
		}
	})

	It("removes nothing when the step was granted no devices", func() {
		// Nothing is written, so nothing can layer; the user's own value stands.
		got := resolved(config.VendorAMD, nil).dropForeignDeviceVars(inherited)
		Expect(got).To(Equal(inherited))
	})

	It("still removes the other vendor's masks under cgroup isolation", func() {
		// Removal and writing are separate concerns. GE's devices_allow makes the
		// runtime enumerate only the granted devices and renumber them from zero,
		// so an inherited mask holding absolute ids indexes into that filtered
		// list and selects the wrong devices -- the failure this exists to stop.
		// Only the variable this site writes is left alone, because under cgroup
		// it is GE's to set.
		s := supWith(config.VendorAMD, []string{"0", "1"})
		s.cfg.GPU.Isolation = "cgroup"
		Expect(s.resolveDeviceVars()).To(Succeed())
		got := s.dropForeignDeviceVars(inherited)
		Expect(got).To(ContainElement("ROCR_VISIBLE_DEVICES=4,5"), "GE's own value stays")
		for _, kv := range got {
			Expect(kv).NotTo(HavePrefix("CUDA_VISIBLE_DEVICES="))
			Expect(kv).NotTo(HavePrefix("HIP_VISIBLE_DEVICES="))
			Expect(kv).NotTo(HavePrefix("GPU_DEVICE_ORDINAL="))
		}
	})

	It("does not refuse a step over the vendor under cgroup isolation", func() {
		// The vendor selects nothing there, so a typo must not fail every GPU step
		// on the cluster over a value that is never read.
		s := supWith("intel", []string{"0", "1"})
		s.cfg.GPU.Isolation = "cgroup"
		Expect(s.resolveDeviceVars()).To(Succeed())
	})

	It("refuses a step over the vendor under shim isolation", func() {
		Expect(supWith("intel", []string{"0", "1"}).resolveDeviceVars()).NotTo(Succeed())
	})

	It("does not refuse a step that holds no devices", func() {
		Expect(supWith("intel", nil).resolveDeviceVars()).To(Succeed())
	})

	It("removes nothing when the vendor is unknown, since the step is refused", func() {
		// resolveDeviceVars errors for this step, so srun exits before baseEnv runs
		// and gpuDrop stays empty.
		s := supWith("intel", []string{"0", "1"})
		Expect(s.resolveDeviceVars()).NotTo(Succeed())
		Expect(s.dropForeignDeviceVars(inherited)).To(Equal(inherited))
	})

	It("never sets a device variable to empty, which means zero devices", func() {
		got := resolved(config.VendorAMD, []string{"0", "1"}).dropForeignDeviceVars(inherited)
		for _, kv := range got {
			Expect(kv).NotTo(Equal("CUDA_VISIBLE_DEVICES="))
			Expect(kv).NotTo(Equal("HIP_VISIBLE_DEVICES="))
		}
	})

	It("leaves an entry whose key merely contains the name as a substring", func() {
		got := dropEnv([]string{"MY_CUDA_VISIBLE_DEVICES=1", "CUDA_VISIBLE_DEVICES=2"},
			proto.EnvCUDADevices)
		Expect(got).To(ConsistOf("MY_CUDA_VISIBLE_DEVICES=1"))
	})
})

var _ = Describe("interactive sessions inherit the login device mask [REQ-GPU-004]", func() {
	// qrsh -V forwards the caller's whole environment and an interactive session
	// never builds a step environment, so none of the per-rank hygiene applies.
	// The user owns that environment, so warn rather than scrub.
	amd := func() *config.Config {
		cfg := config.Default()
		cfg.GPU.Vendor = config.VendorAMD
		return cfg
	}

	It("warns when a stale foreign mask is exported and the session wants GPUs", func() {
		GinkgoT().Setenv(proto.EnvCUDADevices, "6,7")
		w := interactiveDeviceMaskWarning(amd(), true)
		Expect(w).To(ContainSubstring(proto.EnvCUDADevices))
		Expect(w).To(ContainSubstring(proto.EnvROCRDevices))
	})

	It("stays quiet when the session asked for no GPUs", func() {
		GinkgoT().Setenv(proto.EnvCUDADevices, "6,7")
		Expect(interactiveDeviceMaskWarning(amd(), false)).To(BeEmpty())
	})

	It("stays quiet when no foreign mask is present", func() {
		GinkgoT().Setenv(proto.EnvROCRDevices, "0,1")
		Expect(interactiveDeviceMaskWarning(amd(), true)).To(BeEmpty(),
			"the variable this site publishes is not a stale foreign mask")
	})

	It("warns an nvidia site about a stale ROCr mask", func() {
		GinkgoT().Setenv(proto.EnvROCRDevices, "6,7")
		w := interactiveDeviceMaskWarning(config.Default(), true)
		Expect(w).To(ContainSubstring(proto.EnvROCRDevices))
	})
})

var _ = Describe("CUDA_DEVICE_ORDER changes what an id means [REQ-GPU-004]", func() {
	// It is reported, never removed. Dropping it would flip a site that set
	// PCI_BUS_ID precisely to make its ids line up with nvidia-smi order, turning
	// a correct setup into a silently wrong one.
	sup := func(gpus []string) *supervisor {
		s := &supervisor{
			cfg:  config.Default(),
			plan: &plan.StepPlan{Ranks: []plan.PlacedRank{{GPUs: gpus}}},
		}
		Expect(s.resolveDeviceVars()).To(Succeed())
		return s
	}

	It("warns when the order contradicts the id contract", func() {
		GinkgoT().Setenv(proto.EnvCUDADeviceOrder, "FASTEST_FIRST")
		w := sup([]string{"0", "1"}).gpuRequestWarnings(&options{})
		Expect(w).To(ContainElement(ContainSubstring(proto.EnvCUDADeviceOrder)))
	})

	It("stays quiet when the order matches the id contract", func() {
		GinkgoT().Setenv(proto.EnvCUDADeviceOrder, proto.OrderPCIBusID)
		Expect(sup([]string{"0", "1"}).gpuRequestWarnings(&options{})).To(BeEmpty())
	})

	It("accepts the matching value in any case", func() {
		GinkgoT().Setenv(proto.EnvCUDADeviceOrder, "pci_bus_id")
		Expect(sup([]string{"0", "1"}).gpuRequestWarnings(&options{})).To(BeEmpty())
	})

	It("stays quiet on a step with no devices", func() {
		GinkgoT().Setenv(proto.EnvCUDADeviceOrder, "FASTEST_FIRST")
		Expect(sup(nil).gpuRequestWarnings(&options{})).To(BeEmpty())
	})

	It("never removes it from the rank environment", func() {
		s := sup([]string{"0", "1"})
		got := s.dropForeignDeviceVars([]string{proto.EnvCUDADeviceOrder + "=PCI_BUS_ID"})
		Expect(got).To(ContainElement(proto.EnvCUDADeviceOrder + "=PCI_BUS_ID"))
	})
})
