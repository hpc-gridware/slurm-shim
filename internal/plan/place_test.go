package plan_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

func alloc() *layout.Layout {
	return &layout.Layout{
		Nodes: []layout.Node{
			{Index: 0, Host: "node001", Slots: 8, GPUs: []int{0, 1}},
			{Index: 1, Host: "node002", Slots: 8, GPUs: []int{0, 1}},
		},
		Tasks: layout.Tasks{NTasks: 16, CPUsPerTask: 1, PerNode: []int{8, 8}},
	}
}

var _ = Describe("Step placement", func() {
	It("block-distributes -n ranks across both nodes [REQ-RUN-008]", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{NTasks: 16})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.NTasks).To(Equal(16))
		Expect(p.Ranks).To(HaveLen(16))
		// Block: ranks 0-7 on node 0, 8-15 on node 1.
		Expect(p.Ranks[0].StepNodeIndex).To(Equal(0))
		Expect(p.Ranks[7].StepNodeIndex).To(Equal(0))
		Expect(p.Ranks[8].StepNodeIndex).To(Equal(1))
		Expect(p.Ranks[8].Local).To(Equal(0))
	})

	It("defaults to one task per node when only -N is given [REQ-RUN-025]", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{Nodes: 2})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.NTasks).To(Equal(2))
		Expect(p.Ranks[0].StepNodeIndex).To(Equal(0))
		Expect(p.Ranks[1].StepNodeIndex).To(Equal(1))
	})

	It("uses the first -N nodes", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{Nodes: 1, NTasks: 4})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Nodes).To(HaveLen(1))
		Expect(p.Ranks).To(HaveLen(4))
	})

	It("honors --ntasks-per-node as the per-node capacity", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{TasksPerNode: 2})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.NTasks).To(Equal(4))
		Expect(p.Ranks).To(HaveLen(4))
	})

	It("selects a --nodelist subset in allocation order [REQ-RUN-002]", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{Nodelist: []string{"node002"}, NTasks: 3})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Nodes).To(HaveLen(1))
		Expect(p.Nodes[0].Host).To(Equal("node002"))
	})

	It("rejects a --nodelist host outside the allocation [REQ-RUN-002]", func() {
		_, err := plan.Place(alloc(), plan.StepRequest{Nodelist: []string{"node999"}})
		Expect(err).To(HaveOccurred())
	})

	It("fails when more tasks are requested than permitted [REQ-RUN-008]", func() {
		_, err := plan.Place(alloc(), plan.StepRequest{NTasks: 100})
		Expect(err).To(MatchError(ContainSubstring("More processors requested than permitted")))
	})

	It("assigns contiguous cpusets sized by cpus-per-task", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{TasksPerNode: 2, CPUsPerTask: 4})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Ranks[0].Cpuset).To(Equal("0-3"))
		Expect(p.Ranks[1].Cpuset).To(Equal("4-7"))
	})

	It("partitions GPUs per rank and rejects over-subscription [REQ-RUN-008]", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{TasksPerNode: 2, GPUsPerTask: 1})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Ranks[0].GPUs).To(Equal([]int{0}))
		Expect(p.Ranks[1].GPUs).To(Equal([]int{1}))

		_, err = plan.Place(alloc(), plan.StepRequest{TasksPerNode: 1, GPUsPerTask: 4})
		Expect(err).To(MatchError(ContainSubstring("exceeds")))
	})

	It("partitions GPUs per rank by default without --gpus-per-task [REQ-GPU-002]", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{TasksPerNode: 2})
		Expect(err).NotTo(HaveOccurred())
		// 2 GPUs, 2 tasks per node -> one device per rank on each node.
		Expect(p.Ranks[0].GPUs).To(Equal([]int{0}))
		Expect(p.Ranks[1].GPUs).To(Equal([]int{1}))
		Expect(p.Ranks[2].GPUs).To(Equal([]int{0})) // node002, local 0
		Expect(p.Ranks[3].GPUs).To(Equal([]int{1}))
		Expect(p.Warnings).To(BeEmpty())
	})

	It("shares GPUs and warns once when tasks exceed GPUs [REQ-GPU-002]", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{TasksPerNode: 4})
		Expect(err).NotTo(HaveOccurred())
		// 2 GPUs, 4 tasks per node -> every rank sees both devices.
		Expect(p.Ranks[0].GPUs).To(Equal([]int{0, 1}))
		Expect(p.Ranks[3].GPUs).To(Equal([]int{0, 1}))
		Expect(p.Warnings).To(HaveLen(1))
		Expect(p.Warnings[0]).To(ContainSubstring("all ranks share"))
	})

	It("assigns no GPUs when the allocation granted none", func() {
		lay := alloc()
		lay.Nodes[0].GPUs = nil
		lay.Nodes[1].GPUs = nil
		p, err := plan.Place(lay, plan.StepRequest{TasksPerNode: 2})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Ranks[0].GPUs).To(BeEmpty())
		Expect(p.Warnings).To(BeEmpty())
	})
})
