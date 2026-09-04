package submit_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

func TestSubmit(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Submit Suite")
}

// One home for the SLURM->GE conversions both sbatch and interactive srun use,
// so a change cannot land in one command and not the other (todos/011).
var _ = Describe("shared translations", func() {
	DescribeTable("ParseSlurmTime",
		func(v string, want int) {
			s, err := submit.ParseSlurmTime(v)
			Expect(err).NotTo(HaveOccurred())
			Expect(s).To(Equal(want))
		},
		Entry("minutes", "30", 1800),
		Entry("MM:SS", "01:30", 90),
		Entry("HH:MM:SS", "2:00:00", 7200),
		Entry("D-HH", "1-00", 86400),
		Entry("D-HH:MM:SS", "0-12:30:00", 45000),
	)
	It("rejects a non-numeric time with a neutral (prefix-free) error", func() {
		_, err := submit.ParseSlurmTime("2:0x:00")
		Expect(err).To(MatchError(ContainSubstring("--time: invalid")))
		Expect(err.Error()).NotTo(ContainSubstring("sbatch"))
		Expect(err.Error()).NotTo(ContainSubstring("srun"))
	})

	DescribeTable("ConvertMem",
		func(v, want string) { Expect(submit.ConvertMem(v)).To(Equal(want)) },
		Entry("GB->G", "4GB", "4G"), Entry("G stays", "4G", "4G"),
		Entry("bare is MB", "512", "512M"), Entry("empty", "", ""),
	)

	DescribeTable("ExportArgs",
		func(spec string, want []string) { Expect(submit.ExportArgs(spec)).To(Equal(want)) },
		Entry("default ALL", "", []string{"-V"}),
		Entry("NONE", "NONE", []string(nil)),
		Entry("list", "A=1,B=2", []string{"-v", "A=1", "-v", "B=2"}),
		Entry("ALL plus list", "ALL,A=1", []string{"-V", "-v", "A=1"}),
	)

	Describe("Slots + ResourceList over a per-task partition", func() {
		part := config.Partition{Queue: "all.q", PE: "make", Slots: "per-task"}
		cfg := &config.Config{MemoryComplex: "h_vmem", GPU: config.GPU{GresComplex: "gpu"}}

		It("multiplies tasks by cpus-per-task", func() {
			r := submit.Request{NTasks: 2, CPUsPerTask: 4, HaveNTasks: true}
			n, err := submit.Slots(r, part)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(8))
		})
		It("renders h_rt, the memory complex and the gpu complex", func() {
			r := submit.Request{HaveTime: true, TimeSec: 1800, Mem: "2G", HaveGPUs: true, GPUs: 1}
			Expect(submit.ResourceList(cfg, r)).To(Equal("h_rt=1800,h_vmem=2G,gpu=1"))
		})
		It("is empty when nothing was requested", func() {
			Expect(submit.ResourceList(cfg, submit.Request{})).To(Equal(""))
		})
	})
})
