/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

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
			{Index: 0, Host: "node001", Slots: 8, GPUs: []string{"0", "1"}},
			{Index: 1, Host: "node002", Slots: 8, GPUs: []string{"0", "1"}},
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

	Describe("an MPI launcher's daemon step", func() {
		threeNodes := func() *layout.Layout {
			return &layout.Layout{
				Nodes: []layout.Node{
					{Index: 0, Host: "node001", Slots: 4},
					{Index: 1, Host: "node002", Slots: 4},
					{Index: 2, Host: "node003", Slots: 4},
				},
				Tasks: layout.Tasks{CPUsPerTask: 1, PerNode: []int{4, 4, 4}},
			}
		}

		It("keeps the --nodelist order when Ordered, so SLURM_NODEID follows the list", func() {
			p, err := plan.Place(threeNodes(), plan.StepRequest{
				Nodelist: []string{"node003", "node002"}, TasksPerNode: 1, NTasks: 2, Ordered: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Nodes[0].Host).To(Equal("node003"))
			Expect(p.Nodes[1].Host).To(Equal("node002"))
			Expect(p.Ranks[0].StepNodeIndex).To(Equal(0))
			Expect(p.Ranks[1].StepNodeIndex).To(Equal(1))

			p, err = plan.Place(threeNodes(), plan.StepRequest{
				Nodelist: []string{"node003", "node002"}, TasksPerNode: 1, NTasks: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Nodes[0].Host).To(Equal("node002"), "without Ordered, allocation order")
		})

		It("leaves the master out with AvoidMaster, unless it is the only node", func() {
			p, err := plan.Place(threeNodes(), plan.StepRequest{TasksPerNode: 1, NTasks: 2, AvoidMaster: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Nodes).To(HaveLen(2))
			Expect(p.Nodes[0].Host).To(Equal("node002"))
			Expect(p.Nodes[1].Host).To(Equal("node003"))

			single := threeNodes()
			single.Nodes = single.Nodes[:1]
			single.Tasks.PerNode = []int{4}
			p, err = plan.Place(single, plan.StepRequest{NTasks: 1, AvoidMaster: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Nodes[0].Host).To(Equal("node001"))

			p, err = plan.Place(threeNodes(), plan.StepRequest{NTasks: 1, AvoidMaster: true, Exclude: []string{"node002", "node003"}})
			Expect(err).NotTo(HaveOccurred(), "the master is the only node --exclude leaves")
			Expect(p.Nodes[0].Host).To(Equal("node001"))
		})

		It("gives no cpuset with NoBind", func() {
			p, err := plan.Place(threeNodes(), plan.StepRequest{TasksPerNode: 1, NoBind: true})
			Expect(err).NotTo(HaveOccurred())
			for _, r := range p.Ranks {
				Expect(r.Cpuset).To(BeEmpty())
			}
			p, err = plan.Place(threeNodes(), plan.StepRequest{TasksPerNode: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Ranks[0].Cpuset).To(Equal("0-0"), "an ordinary step keeps its cpuset")
		})
	})

	It("leaves --exclude hosts out of the step", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{Exclude: []string{"node001"}, NTasks: 2})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Nodes).To(HaveLen(1))
		Expect(p.Nodes[0].Host).To(Equal("node002"))
	})

	It("refuses an --exclude that leaves no node", func() {
		_, err := plan.Place(alloc(), plan.StepRequest{Exclude: []string{"node001", "node002"}})
		Expect(err).To(MatchError(ContainSubstring("leaves no node")))
		_, err = plan.Place(alloc(), plan.StepRequest{Nodelist: []string{"node002"}, Exclude: []string{"node002"}})
		Expect(err).To(MatchError(ContainSubstring("leave no node")))
	})

	It("counts -N from the hosts --exclude leaves", func() {
		_, err := plan.Place(alloc(), plan.StepRequest{Nodes: 2, Exclude: []string{"node001"}})
		Expect(err).To(MatchError(ContainSubstring("requested 2 nodes, allocation has 1")))
	})

	It("rejects a --nodelist host outside the allocation [REQ-RUN-002]", func() {
		_, err := plan.Place(alloc(), plan.StepRequest{Nodelist: []string{"node999"}})
		Expect(err).To(HaveOccurred())
	})

	Describe("spreading -n over the step nodes as SLURM does", func() {
		// hosts and counts of a plan, in step order, for compact expectations.
		perNode := func(p *plan.StepPlan) map[string]int {
			m := map[string]int{}
			for _, r := range p.Ranks {
				m[p.Nodes[r.StepNodeIndex].Host]++
			}
			return m
		}
		threeNodes := func() *layout.Layout {
			return &layout.Layout{
				Nodes: []layout.Node{
					{Index: 0, Host: "node001", Slots: 2},
					{Index: 1, Host: "node002", Slots: 2},
					{Index: 2, Host: "node003", Slots: 2},
				},
				Tasks: layout.Tasks{NTasks: 6, CPUsPerTask: 1, PerNode: []int{2, 2, 2}},
			}
		}

		It("runs -N 2 -n 2 one rank per node, not both on the first", func() {
			p, err := plan.Place(threeNodes(), plan.StepRequest{Nodes: 2, NTasks: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Nodes).To(HaveLen(2))
			Expect(p.Ranks).To(Equal([]plan.PlacedRank{
				{Rank: 0, StepNodeIndex: 0, Local: 0, Cpuset: "0-0"},
				{Rank: 1, StepNodeIndex: 1, Local: 0, Cpuset: "0-0"},
			}))
			Expect(p.Warnings).To(BeEmpty())
		})

		It("numbers the ranks in block order when the nodes run unequal counts", func() {
			p, err := plan.Place(threeNodes(), plan.StepRequest{Nodes: 2, NTasks: 3})
			Expect(err).NotTo(HaveOccurred())
			Expect(perNode(p)).To(Equal(map[string]int{"node001": 2, "node002": 1}))
			Expect(p.Ranks[0].StepNodeIndex).To(Equal(0))
			Expect(p.Ranks[1].StepNodeIndex).To(Equal(0))
			Expect(p.Ranks[1].Local).To(Equal(1))
			Expect(p.Ranks[2].StepNodeIndex).To(Equal(1))
			Expect(p.Ranks[2].Local).To(Equal(0))
		})

		It("balances the counts rather than filling the first node", func() {
			// SLURM: -N 2 -n 4 on 8-CPU nodes is 2 and 2, not 3 and 1 or 4 and 0.
			p, err := plan.Place(alloc(), plan.StepRequest{Nodes: 2, NTasks: 4})
			Expect(err).NotTo(HaveOccurred())
			Expect(perNode(p)).To(Equal(map[string]int{"node001": 2, "node002": 2}))
		})

		It("spreads over every allocation node when -N is not given", func() {
			// srun inherits the allocation's node count (SLURM_NNODES).
			p, err := plan.Place(alloc(), plan.StepRequest{NTasks: 4})
			Expect(err).NotTo(HaveOccurred())
			Expect(perNode(p)).To(Equal(map[string]int{"node001": 2, "node002": 2}))

			p, err = plan.Place(threeNodes(), plan.StepRequest{NTasks: 3})
			Expect(err).NotTo(HaveOccurred())
			Expect(perNode(p)).To(Equal(map[string]int{"node001": 1, "node002": 1, "node003": 1}))
		})

		It("moves on to the nodes with room once a node is full", func() {
			lay := threeNodes()
			lay.Tasks.PerNode = []int{4, 1, 2}
			p, err := plan.Place(lay, plan.StepRequest{NTasks: 6})
			Expect(err).NotTo(HaveOccurred())
			Expect(perNode(p)).To(Equal(map[string]int{"node001": 3, "node002": 1, "node003": 2}))
		})

		It("gives every node of a -w list a rank, in list order under --distribution=arbitrary", func() {
			p, err := plan.Place(threeNodes(), plan.StepRequest{Nodelist: []string{"node003", "node001"}, NTasks: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(perNode(p)).To(Equal(map[string]int{"node001": 1, "node003": 1}))
			Expect(p.Nodes[0].Host).To(Equal("node001"))

			p, err = plan.Place(threeNodes(), plan.StepRequest{Nodelist: []string{"node003", "node001"}, NTasks: 2, Ordered: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Nodes[0].Host).To(Equal("node003"))
			Expect(p.Ranks[0].StepNodeIndex).To(Equal(0))
			Expect(p.Ranks[1].StepNodeIndex).To(Equal(1))
		})

		It("shrinks the step to -n nodes with SLURM's warning when -n is below the node count", func() {
			p, err := plan.Place(threeNodes(), plan.StepRequest{Nodes: 2, NTasks: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Nodes).To(HaveLen(1))
			Expect(p.Nodes[0].Host).To(Equal("node001"))
			Expect(p.Warnings).To(Equal([]string{
				"srun: Warning: can't run 1 processes on 2 nodes, setting nnodes to 1"}))

			p, err = plan.Place(threeNodes(), plan.StepRequest{NTasks: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(perNode(p)).To(Equal(map[string]int{"node001": 1, "node002": 1}))
			Expect(p.Warnings).To(Equal([]string{
				"srun: Warning: can't run 2 processes on 3 nodes, setting nnodes to 2"}))
		})

		It("keeps no node that runs no rank", func() {
			// A GPU task policy gives a node without GPUs no tasks; it must not
			// count in the step or get a stepper.
			lay := threeNodes()
			lay.Tasks.PerNode = []int{2, 0, 2}
			p, err := plan.Place(lay, plan.StepRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Nodes).To(HaveLen(2))
			Expect(p.Nodes[1].Host).To(Equal("node003"))
			Expect(p.Nodes[1].LayoutIndex).To(Equal(2))
			Expect(p.Ranks[3].StepNodeIndex).To(Equal(1), "step node indexes stay contiguous")
			Expect(p.Warnings).To(BeEmpty())
		})

		It("starts each node's cpusets and GPUs at local rank 0", func() {
			p, err := plan.Place(alloc(), plan.StepRequest{Nodes: 2, NTasks: 4, CPUsPerTask: 2, GPUsPerTask: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Ranks[2].StepNodeIndex).To(Equal(1))
			Expect(p.Ranks[2].Local).To(Equal(0))
			Expect(p.Ranks[2].Cpuset).To(Equal("0-1"))
			Expect(p.Ranks[3].Cpuset).To(Equal("2-3"))
			Expect(p.Ranks[2].GPUs).To(Equal([]string{"0"}))
			Expect(p.Ranks[3].GPUs).To(Equal([]string{"1"}))
		})

		It("leaves one-daemon-per-node launcher steps unchanged", func() {
			// OpenMPI's mpirun starts its daemons with --ntasks-per-node=1.
			p, err := plan.Place(threeNodes(), plan.StepRequest{TasksPerNode: 1, NTasks: 2, AvoidMaster: true, NoBind: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(perNode(p)).To(Equal(map[string]int{"node002": 1, "node003": 1}))
			Expect(p.Warnings).To(BeEmpty())
		})

		It("still refuses more ranks than the step nodes hold [REQ-RUN-008]", func() {
			_, err := plan.Place(threeNodes(), plan.StepRequest{Nodes: 2, NTasks: 5})
			Expect(err).To(MatchError(ContainSubstring("(5 > 4)")))
		})
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
		Expect(p.Ranks[0].GPUs).To(Equal([]string{"0"}))
		Expect(p.Ranks[1].GPUs).To(Equal([]string{"1"}))

		_, err = plan.Place(alloc(), plan.StepRequest{TasksPerNode: 1, GPUsPerTask: 4})
		Expect(err).To(MatchError(ContainSubstring("exceeds")))
	})

	It("keeps the node's whole grant visible to every rank by default [REQ-GPU-002]", func() {
		// SLURM does not bind tasks to a device subset unless asked, and JAX
		// (local_device_ids=[SLURM_LOCALID]) and torch (LOCAL_RANK) index into
		// the visible list, so every rank must see all of it.
		p, err := plan.Place(alloc(), plan.StepRequest{TasksPerNode: 2})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Ranks[0].GPUs).To(Equal([]string{"0", "1"}))
		Expect(p.Ranks[1].GPUs).To(Equal([]string{"0", "1"}))
		Expect(p.Ranks[2].GPUs).To(Equal([]string{"0", "1"})) // node002, local 0
		Expect(p.Ranks[3].GPUs).To(Equal([]string{"0", "1"}))
		// The one notice marks where this differs from earlier shim releases.
		Expect(p.Warnings).To(HaveLen(1))
		Expect(p.Warnings[0]).To(ContainSubstring("SLURM default"))
	})

	It("partitions per rank under legacy auto-division [REQ-GPU-002]", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{TasksPerNode: 2, AutoDivideGPUs: true})
		Expect(err).NotTo(HaveOccurred())
		// 2 GPUs, 2 tasks per node -> one device per rank on each node.
		Expect(p.Ranks[0].GPUs).To(Equal([]string{"0"}))
		Expect(p.Ranks[1].GPUs).To(Equal([]string{"1"}))
		Expect(p.Ranks[2].GPUs).To(Equal([]string{"0"})) // node002, local 0
		Expect(p.Ranks[3].GPUs).To(Equal([]string{"1"}))
		Expect(p.Warnings).To(BeEmpty())
	})

	It("shares GPUs and warns once when tasks exceed GPUs under auto-division [REQ-GPU-002]", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{TasksPerNode: 4, AutoDivideGPUs: true})
		Expect(err).NotTo(HaveOccurred())
		// 2 GPUs, 4 tasks per node -> every rank sees both devices.
		Expect(p.Ranks[0].GPUs).To(Equal([]string{"0", "1"}))
		Expect(p.Ranks[3].GPUs).To(Equal([]string{"0", "1"}))
		Expect(p.Warnings).To(HaveLen(1))
		Expect(p.Warnings[0]).To(ContainSubstring("all ranks share"))
	})

	It("suppresses the notice when the binding was requested explicitly", func() {
		// The notice advises how to bind; a user who passed --gpu-bind=none has
		// already declined that advice.
		p, err := plan.Place(alloc(), plan.StepRequest{TasksPerNode: 2, GPUBindExplicit: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Ranks[0].GPUs).To(Equal([]string{"0", "1"}))
		Expect(p.Warnings).To(BeEmpty())
	})

	It("emits no notice when fewer GPUs than ranks (behavior unchanged there)", func() {
		p, err := plan.Place(alloc(), plan.StepRequest{TasksPerNode: 4})
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Ranks[0].GPUs).To(Equal([]string{"0", "1"}))
		Expect(p.Warnings).To(BeEmpty())
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
