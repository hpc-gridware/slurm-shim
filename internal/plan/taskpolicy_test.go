package plan_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

var _ = Describe("N3 task policy", func() {
	homogeneous := []plan.NodeAlloc{{Slots: 8, GPUs: 2}, {Slots: 8, GPUs: 2}}

	Describe("slot policy [REQ-ENC-004]", func() {
		It("yields one task per slot and omits cpus_per_task", func() {
			g, err := plan.ApplyPolicy(plan.PolicySlot, homogeneous)
			Expect(err).NotTo(HaveOccurred())
			Expect(g.NTasks).To(Equal(16))
			Expect(g.PerNode).To(Equal([]int{8, 8}))
			Expect(g.CPUsPerTaskSet).To(BeFalse())
		})
	})

	Describe("node policy [REQ-ENC-004]", func() {
		It("yields one task per node with per-node slots as cpus_per_task", func() {
			g, err := plan.ApplyPolicy(plan.PolicyNode, homogeneous)
			Expect(err).NotTo(HaveOccurred())
			Expect(g.NTasks).To(Equal(2))
			Expect(g.PerNode).To(Equal([]int{1, 1}))
			Expect(g.CPUsPerTask).To(Equal(8))
			Expect(g.Warnings).To(BeEmpty())
		})

		It("collapses heterogeneous slots to the minimum with a warning [REQ-ENC-004]", func() {
			g, err := plan.ApplyPolicy(plan.PolicyNode, []plan.NodeAlloc{{Slots: 8}, {Slots: 4}})
			Expect(err).NotTo(HaveOccurred())
			Expect(g.CPUsPerTask).To(Equal(4))
			Expect(g.Warnings).To(HaveLen(1))
		})
	})

	Describe("gpu policy [REQ-ENC-005]", func() {
		It("yields one task per GPU with floor(slots/gpus) cpus_per_task", func() {
			g, err := plan.ApplyPolicy(plan.PolicyGPU, homogeneous)
			Expect(err).NotTo(HaveOccurred())
			Expect(g.NTasks).To(Equal(4))
			Expect(g.PerNode).To(Equal([]int{2, 2}))
			Expect(g.CPUsPerTask).To(Equal(4))
		})

		It("gives GPU-less nodes zero tasks [REQ-ENC-005]", func() {
			g, err := plan.ApplyPolicy(plan.PolicyGPU, []plan.NodeAlloc{{Slots: 8, GPUs: 2}, {Slots: 8, GPUs: 0}})
			Expect(err).NotTo(HaveOccurred())
			Expect(g.PerNode).To(Equal([]int{2, 0}))
			Expect(g.NTasks).To(Equal(2))
		})

		It("warns when slots-per-gpu is heterogeneous [REQ-ENC-004]", func() {
			g, err := plan.ApplyPolicy(plan.PolicyGPU, []plan.NodeAlloc{{Slots: 8, GPUs: 2}, {Slots: 8, GPUs: 4}})
			Expect(err).NotTo(HaveOccurred())
			Expect(g.CPUsPerTask).To(Equal(2))
			Expect(g.Warnings).To(HaveLen(1))
		})

		It("fails when no node has a GPU [REQ-ENC-005]", func() {
			_, err := plan.ApplyPolicy(plan.PolicyGPU, []plan.NodeAlloc{{Slots: 8, GPUs: 0}})
			Expect(err).To(MatchError(plan.ErrNoGPUs))
		})
	})

	It("rejects an unknown policy and an empty allocation", func() {
		_, err := plan.ApplyPolicy("bogus", homogeneous)
		Expect(err).To(HaveOccurred())
		_, err = plan.ApplyPolicy(plan.PolicySlot, nil)
		Expect(err).To(HaveOccurred())
	})
})
