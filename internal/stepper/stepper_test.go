package stepper

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
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

var _ = Describe("rank device variable [REQ-GPU-004]", func() {
	withGPUs := proto.RankSpec{Rank: 0, GPUs: []string{"2", "3"}, EnvDelta: []string{"SLURM_PROCID=0"}}

	// The NVIDIA path had no direct coverage before AMD support was added. These
	// two pin the pre-existing behaviour so the vendor work cannot quietly change
	// it: an empty name is what every StepSpec written by an older srun carries.
	It("defaults to CUDA_VISIBLE_DEVICES when no variable name is given", func() {
		got, err := RankOverlay(withGPUs, "node001", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(ContainElement("CUDA_VISIBLE_DEVICES=2,3"))
		Expect(got).To(ContainElement("SLURMD_NODENAME=node001"))
		Expect(got).To(ContainElement("SLURM_PROCID=0"))
	})

	It("writes CUDA_VISIBLE_DEVICES when the name is given explicitly", func() {
		got, err := RankOverlay(withGPUs, "node001", proto.EnvCUDADevices)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(ContainElement("CUDA_VISIBLE_DEVICES=2,3"))
	})

	It("writes ROCR_VISIBLE_DEVICES on AMD, and nothing else", func() {
		got, err := RankOverlay(withGPUs, "node001", proto.EnvROCRDevices)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(ContainElement("ROCR_VISIBLE_DEVICES=2,3"))
		// Exactly one mask: on ROCm the HIP-level variables index into the list
		// ROCR_VISIBLE_DEVICES already filtered, so a second mask holding the same
		// absolute ids selects the wrong devices or none at all.
		for _, kv := range got {
			Expect(kv).NotTo(HavePrefix("CUDA_VISIBLE_DEVICES="))
			Expect(kv).NotTo(HavePrefix("HIP_VISIBLE_DEVICES="))
			Expect(kv).NotTo(HavePrefix("GPU_DEVICE_ORDINAL="))
		}
	})

	It("writes no device variable when the rank was granted none", func() {
		got, err := RankOverlay(proto.RankSpec{Rank: 1}, "node001", proto.EnvROCRDevices)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(ContainElement("SLURMD_NODENAME=node001"))
		for _, kv := range got {
			Expect(kv).NotTo(HavePrefix("ROCR_VISIBLE_DEVICES="))
		}
	})

	// Fail closed. An unrecognised name can only come from a newer srun this
	// stepper does not understand, and guessing would hand the rank a silently
	// wrong device set -- the exact failure the vendor split exists to prevent.
	It("refuses an unrecognised variable name rather than guessing", func() {
		_, err := RankOverlay(withGPUs, "node001", "ZE_AFFINITY_MASK")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("ZE_AFFINITY_MASK"))
	})

	// dedupEnv keys on the first "=", so a name carrying one would shadow an
	// unrelated variable instead of setting a device mask.
	It("refuses a name containing an equals sign", func() {
		_, err := RankOverlay(withGPUs, "node001", "PATH=/tmp/evil:")
		Expect(err).To(HaveOccurred())
	})

	It("layers the device variable over the step environment", func() {
		spec := proto.StepSpec{
			Env:       []string{"HOME=/h", "CUDA_VISIBLE_DEVICES=9"},
			GPUEnvVar: proto.EnvCUDADevices,
		}
		got, err := rankEnv(spec, withGPUs, "node001")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(ContainElement("CUDA_VISIBLE_DEVICES=2,3"))
		Expect(got).NotTo(ContainElement("CUDA_VISIBLE_DEVICES=9"))
		Expect(got).To(ContainElement("HOME=/h"))
	})

	It("propagates the refusal out of rankEnv", func() {
		spec := proto.StepSpec{Env: []string{"HOME=/h"}, GPUEnvVar: "NOPE"}
		_, err := rankEnv(spec, withGPUs, "node001")
		Expect(err).To(HaveOccurred())
	})
})
