package plan_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

var _ = Describe("AssignDevices [REQ-GPU-002]", func() {
	It("partitions contiguously by floor(gpus/localTasks) without a per-task flag", func() {
		perRank, shared := plan.AssignDevices([]int{0, 1, 2, 3}, 2, 0)
		Expect(shared).To(BeFalse())
		Expect(perRank).To(Equal([][]int{{0, 1}, {2, 3}}))
	})

	It("gives one device per rank for a 2-GPU 2-task node", func() {
		perRank, shared := plan.AssignDevices([]int{0, 1}, 2, 0)
		Expect(shared).To(BeFalse())
		Expect(perRank).To(Equal([][]int{{0}, {1}}))
	})

	It("honors an explicit gpus-per-task", func() {
		perRank, shared := plan.AssignDevices([]int{0, 1, 2, 3}, 2, 2)
		Expect(shared).To(BeFalse())
		Expect(perRank).To(Equal([][]int{{0, 1}, {2, 3}}))
	})

	It("shares the full list when there are fewer GPUs than tasks", func() {
		perRank, shared := plan.AssignDevices([]int{0}, 4, 0)
		Expect(shared).To(BeTrue())
		Expect(perRank).To(Equal([][]int{{0}, {0}, {0}, {0}}))
	})

	It("does not report sharing for a node with zero GPUs", func() {
		perRank, shared := plan.AssignDevices(nil, 4, 0)
		Expect(shared).To(BeFalse())
		Expect(perRank).To(Equal([][]int{{}, {}, {}, {}}))
	})

	It("gives the last rank the short tail for a non-multiple gpus-per-task", func() {
		perRank, shared := plan.AssignDevices([]int{0, 1, 2}, 2, 2)
		Expect(shared).To(BeFalse())
		Expect(perRank).To(Equal([][]int{{0, 1}, {2}}))
	})

	It("gives ranks past the device count an empty set", func() {
		perRank, _ := plan.AssignDevices([]int{0, 1}, 3, 1)
		Expect(perRank).To(Equal([][]int{{0}, {1}, {}}))
	})

	It("treats a non-positive localTasks as one rank", func() {
		perRank, _ := plan.AssignDevices([]int{0, 1}, 0, 0)
		Expect(perRank).To(Equal([][]int{{0, 1}}))
	})
})
