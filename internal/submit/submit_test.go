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
		Entry("explicit zero-day D-HH:MM", "0-12:30", 45000), // not MM:SS (would be 750)
	)

	DescribeTable("GresGPUCount",
		func(v string, wantN int, wantOK bool) {
			n, ok := submit.GresGPUCount(v)
			Expect(ok).To(Equal(wantOK))
			Expect(n).To(Equal(wantN))
		},
		Entry("gpu:2", "gpu:2", 2, true),
		Entry("typed gpu", "gpu:a100:4", 4, true),
		Entry("gpu without a count is one", "gpu", 1, true),
		Entry("non-gpu gres", "mps:100", 0, false),
	)
	It("rejects a non-numeric time with a neutral (prefix-free) error", func() {
		_, err := submit.ParseSlurmTime("2:0x:00")
		Expect(err).To(MatchError(ContainSubstring("--time: invalid")))
		Expect(err.Error()).NotTo(ContainSubstring("sbatch"))
		Expect(err.Error()).NotTo(ContainSubstring("srun"))
	})

	DescribeTable("ConvertMem",
		func(v, want string) {
			got, err := submit.ConvertMem(v)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("GB->G", "4GB", "4G"), Entry("G stays", "4G", "4G"),
		Entry("MB", "512MB", "512M"), Entry("lower case", "8g", "8G"),
		Entry("bare is MB", "512", "512M"), Entry("a fraction", "1.5G", "1.5G"),
		Entry("empty", "", ""),
	)

	DescribeTable("ConvertMem refuses what is not a size",
		func(v string) {
			_, err := submit.ConvertMem(v)
			Expect(err).To(MatchError(ContainSubstring("invalid memory value")))
		},
		Entry("an extra -l request", "1G,h_rt=99999"),
		Entry("a unit GE does not know", "4P"),
		Entry("a negative size", "-1G"),
		Entry("words", "lots"),
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
