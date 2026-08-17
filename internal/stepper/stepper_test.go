package stepper

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestStepper(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Stepper Suite")
}

var _ = Describe("cpu list parsing", func() {
	DescribeTable("parses ranges and singletons [REQ-STP-002]",
		func(in string, want []int) {
			got, err := parseCPUList(in)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("range", "0-3", []int{0, 1, 2, 3}),
		Entry("range plus single", "0-1,4", []int{0, 1, 4}),
		Entry("single", "5", []int{5}),
	)

	It("rejects a malformed cpu list", func() {
		_, err := parseCPUList("3-1")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("rank-exec argument parsing", func() {
	It("parses cpuset, chdir, and the command after --", func() {
		cpuset, chdir, cmd, err := parseRankExecArgs([]string{"--cpuset", "0-3", "--chdir", "/w", "--", "echo", "hi"})
		Expect(err).NotTo(HaveOccurred())
		Expect(cpuset).To(Equal("0-3"))
		Expect(chdir).To(Equal("/w"))
		Expect(cmd).To(Equal([]string{"echo", "hi"}))
	})

	It("requires a command after --", func() {
		_, _, _, err := parseRankExecArgs([]string{"--"})
		Expect(err).To(HaveOccurred())
	})

	It("requires the -- separator", func() {
		_, _, _, err := parseRankExecArgs([]string{"echo"})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("rank environment layering [REQ-ENV-041]", func() {
	It("shadows a base value with the per-rank delta", func() {
		spec := struct{}{}
		_ = spec
		got := dedupEnv([]string{"SLURM_NODEID=0", "HOME=/h"}, []string{"SLURM_NODEID=1", "SLURM_PROCID=5"})
		Expect(got).To(ContainElement("SLURM_NODEID=1"))
		Expect(got).To(ContainElement("SLURM_PROCID=5"))
		Expect(got).To(ContainElement("HOME=/h"))
		Expect(got).NotTo(ContainElement("SLURM_NODEID=0"))
	})
})
