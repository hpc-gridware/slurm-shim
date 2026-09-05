package submit_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

var _ = Describe("MemoryComplexWarning [address-space complex on a GPU job]", func() {
	gpuReq := submit.Request{Mem: "16G", HaveGPUs: true, GPUs: 1}

	It("warns for every address-space enforced complex when GPUs are requested", func() {
		for _, c := range []string{"h_vmem", "s_vmem", "h_data", "s_data", "h_as", "s_as"} {
			w := submit.MemoryComplexWarning(&config.Config{MemoryComplex: c}, gpuReq)
			Expect(w).To(ContainSubstring("virtual address space"), "complex %s", c)
			Expect(w).To(ContainSubstring("mem_free"), "complex %s must name the fix", c)
			Expect(w).To(ContainSubstring(c), "complex %s must be named", c)
		}
	})

	It("stays silent for a resident/reservation complex", func() {
		for _, c := range []string{"mem_free", "h_rss", "virtual_free", ""} {
			Expect(submit.MemoryComplexWarning(&config.Config{MemoryComplex: c}, gpuReq)).
				To(BeEmpty(), "complex %q", c)
		}
	})

	It("stays silent on a CPU job: h_vmem is a legitimate choice without a CUDA context", func() {
		cpu := submit.Request{Mem: "16G"}
		Expect(submit.MemoryComplexWarning(&config.Config{MemoryComplex: "h_vmem"}, cpu)).To(BeEmpty())
	})

	It("stays silent when no memory was requested (nothing is capped)", func() {
		noMem := submit.Request{HaveGPUs: true, GPUs: 1}
		Expect(submit.MemoryComplexWarning(&config.Config{MemoryComplex: "h_vmem"}, noMem)).To(BeEmpty())
	})
})

var _ = Describe("the shipped memory_complex default", func() {
	It("is mem_free: h_vmem caps RLIMIT_AS and kills CUDA at init", func() {
		Expect(config.Default().MemoryComplex).To(Equal("mem_free"))
	})

	It("is not an address-space enforced complex", func() {
		Expect(submit.MemoryComplexWarning(&config.Config{MemoryComplex: config.Default().MemoryComplex},
			submit.Request{Mem: "16G", HaveGPUs: true, GPUs: 8})).To(BeEmpty())
	})
})

var _ = Describe("VerifyGeometry [-w e cannot see load-sensor complexes]", func() {
	It("suppresses -w e for a load-sensor complex with a memory request", func() {
		for _, c := range []string{"mem_free", "virtual_free", "swap_free"} {
			Expect(submit.VerifyGeometry(&config.Config{MemoryComplex: c}, submit.Request{Mem: "16G"})).
				To(BeFalse(), "complex %s: -w e would refuse a runnable job", c)
		}
	})

	It("keeps -w e when the complex is one -w e can judge", func() {
		for _, c := range []string{"h_vmem", "h_rss"} {
			Expect(submit.VerifyGeometry(&config.Config{MemoryComplex: c}, submit.Request{Mem: "16G"})).
				To(BeTrue(), "complex %s", c)
		}
	})

	It("keeps -w e when no memory was requested (nothing for -w e to miss)", func() {
		Expect(submit.VerifyGeometry(&config.Config{MemoryComplex: "mem_free"}, submit.Request{})).To(BeTrue())
	})

	It("keeps -w e when the memory complex is disabled", func() {
		Expect(submit.VerifyGeometry(&config.Config{MemoryComplex: ""}, submit.Request{Mem: "16G"})).To(BeTrue())
	})

	It("suppresses it under the shipped default, which is load-sensor based", func() {
		Expect(submit.VerifyGeometry(&config.Config{MemoryComplex: config.Default().MemoryComplex},
			submit.Request{Mem: "16G"})).To(BeFalse())
	})
})
