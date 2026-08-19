package plan_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

var _ = Describe("AssignDevices [REQ-GPU-002]", func() {
	Describe("SLURM default: no explicit binding request", func() {
		It("leaves the node's whole grant visible to every rank", func() {
			perRank, shared := plan.AssignDevices([]int{0, 1, 2, 3}, 2, 0, false)
			Expect(shared).To(BeFalse())
			Expect(perRank).To(Equal([][]int{{0, 1, 2, 3}, {0, 1, 2, 3}}))
		})

		It("keeps both devices visible on a 2-GPU 2-task node", func() {
			perRank, shared := plan.AssignDevices([]int{0, 1}, 2, 0, false)
			Expect(shared).To(BeFalse())
			Expect(perRank).To(Equal([][]int{{0, 1}, {0, 1}}))
		})

		It("satisfies the local-rank indexing invariant frameworks rely on", func() {
			// JAX uses local_device_ids=[SLURM_LOCALID]; torch uses LOCAL_RANK.
			// Every local rank must be a valid index into its own visible list.
			perRank, _ := plan.AssignDevices([]int{0, 1, 2, 3, 4, 5, 6, 7}, 8, 0, false)
			for localID, devices := range perRank {
				Expect(len(devices)).To(BeNumerically(">", localID),
					"local rank %d must be able to index its visible devices", localID)
			}
		})

		It("keeps a partial grant intact so rank indexing stays correct", func() {
			perRank, _ := plan.AssignDevices([]int{2, 3}, 2, 0, false)
			Expect(perRank).To(Equal([][]int{{2, 3}, {2, 3}}))
		})

		It("does not report sharing for a node with zero GPUs", func() {
			perRank, shared := plan.AssignDevices(nil, 4, 0, false)
			Expect(shared).To(BeFalse())
			Expect(perRank).To(HaveLen(4))
			for _, d := range perRank {
				Expect(d).To(BeEmpty())
			}
		})
	})

	Describe("explicit --gpus-per-task still binds", func() {
		It("honors an explicit gpus-per-task", func() {
			perRank, shared := plan.AssignDevices([]int{0, 1, 2, 3}, 2, 2, false)
			Expect(shared).To(BeFalse())
			Expect(perRank).To(Equal([][]int{{0, 1}, {2, 3}}))
		})

		It("gives the last rank the short tail for a non-multiple gpus-per-task", func() {
			perRank, shared := plan.AssignDevices([]int{0, 1, 2}, 2, 2, false)
			Expect(shared).To(BeFalse())
			Expect(perRank).To(Equal([][]int{{0, 1}, {2}}))
		})

		It("gives ranks past the device count an empty set", func() {
			perRank, _ := plan.AssignDevices([]int{0, 1}, 3, 1, false)
			Expect(perRank).To(Equal([][]int{{0}, {1}, {}}))
		})
	})

	Describe("legacy auto-division (gpu.bind: per-task)", func() {
		It("partitions contiguously by floor(gpus/localTasks)", func() {
			perRank, shared := plan.AssignDevices([]int{0, 1, 2, 3}, 2, 0, true)
			Expect(shared).To(BeFalse())
			Expect(perRank).To(Equal([][]int{{0, 1}, {2, 3}}))
		})

		It("gives one device per rank for a 2-GPU 2-task node", func() {
			perRank, shared := plan.AssignDevices([]int{0, 1}, 2, 0, true)
			Expect(shared).To(BeFalse())
			Expect(perRank).To(Equal([][]int{{0}, {1}}))
		})

		It("shares the full list when there are fewer GPUs than tasks", func() {
			perRank, shared := plan.AssignDevices([]int{0}, 4, 0, true)
			Expect(shared).To(BeTrue())
			Expect(perRank).To(Equal([][]int{{0}, {0}, {0}, {0}}))
		})
	})

	It("treats a non-positive localTasks as one rank", func() {
		perRank, _ := plan.AssignDevices([]int{0, 1}, 0, 0, false)
		Expect(perRank).To(Equal([][]int{{0, 1}}))
	})
})
