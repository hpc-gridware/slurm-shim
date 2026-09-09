package proto_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// oldRankSpec is the wire shape of a stepper built before device ids became
// strings. The stepper runs from srun's own binary path on each exec host and the
// installer copies that binary per node, so a partial upgrade really can pair a
// new srun with this decoder.
type oldRankSpec struct {
	Rank int   `json:"rank"`
	GPUs []int `json:"gpus"`
}

var _ = Describe("device id wire compatibility [REQ-GPU-004]", func() {
	Describe("a new srun talking to an older stepper", func() {
		It("emits numbers for a numeric grant, so the old decoder still works", func() {
			b, err := json.Marshal(proto.RankSpec{Rank: 0, GPUs: proto.DeviceIDs{"0", "1"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(b)).To(ContainSubstring(`"gpus":[0,1]`))

			var old oldRankSpec
			Expect(json.Unmarshal(b, &old)).To(Succeed())
			Expect(old.GPUs).To(Equal([]int{0, 1}))
		})

		It("emits strings only for a grant that genuinely needs them", func() {
			// A UUID grant cannot be represented as numbers, so this pairing does
			// need matching binaries. That is acceptable: it is opt-in, whereas a
			// bare type change would have broken every numeric site as well.
			b, err := json.Marshal(proto.RankSpec{GPUs: proto.DeviceIDs{"GPU-0123456789abcdef"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(b)).To(ContainSubstring(`"gpus":["GPU-0123456789abcdef"]`))
		})

		It("emits an empty grant in a form the old decoder accepts", func() {
			b, err := json.Marshal(proto.RankSpec{Rank: 1})
			Expect(err).NotTo(HaveOccurred())
			var old oldRankSpec
			Expect(json.Unmarshal(b, &old)).To(Succeed())
			Expect(old.GPUs).To(BeEmpty())
		})
	})

	Describe("a new stepper reading an older srun", func() {
		It("accepts a number array", func() {
			b, err := json.Marshal(oldRankSpec{Rank: 0, GPUs: []int{2, 3}})
			Expect(err).NotTo(HaveOccurred())
			var rs proto.RankSpec
			Expect(json.Unmarshal(b, &rs)).To(Succeed())
			Expect(rs.GPUs).To(Equal(proto.DeviceIDs{"2", "3"}))
		})
	})

	It("round-trips a mixed grant through the full StepSpec", func() {
		spec := proto.StepSpec{
			Env:       []string{"HOME=/h"},
			GPUEnvVar: proto.EnvROCRDevices,
			Ranks: []proto.RankSpec{
				{Rank: 0, GPUs: proto.DeviceIDs{"GPU-0123456789abcdef", "1"}},
			},
		}
		b, err := proto.EncodeSpec(spec)
		Expect(err).NotTo(HaveOccurred())
		got, err := proto.DecodeSpec(b)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(spec))
		Expect(got.GPUEnvVar).To(Equal(proto.EnvROCRDevices), "the vendor must survive the wire")
	})

	It("rejects a device id that is neither a number nor a string", func() {
		var rs proto.RankSpec
		Expect(json.Unmarshal([]byte(`{"gpus":[true]}`), &rs)).To(HaveOccurred())
	})

	It("keeps a nil grant nil rather than turning it into an empty list", func() {
		var rs proto.RankSpec
		Expect(json.Unmarshal([]byte(`{"gpus":null}`), &rs)).To(Succeed())
		Expect(rs.GPUs).To(BeNil())
	})
})
