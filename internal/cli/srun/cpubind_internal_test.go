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

package srun

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/cpubind"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

// The master-side half of the pinning rule (CS-2945): what srun tells each
// host's stepper, and what it tells OpenMPI.
var _ = Describe("CPU binding", func() {
	Describe("the step spec each host receives", func() {
		lay := func() *layout.Layout {
			return &layout.Layout{
				Nodes: []layout.Node{
					// More slots than this step's tasks: NodeSlots must be the
					// job's slots on the node, not the step's task count there.
					{Index: 0, Host: "node001", Slots: 8, IsMaster: true, HostSlots: 8},
					{Index: 1, Host: "node002", Slots: 5, HostSlots: 16},
				},
				Tasks: layout.Tasks{NTasks: 6, CPUsPerTask: 1, PerNode: []int{2, 4}},
			}
		}
		sup := func(mode string) *supervisor {
			l := lay()
			p, err := plan.Place(l, plan.StepRequest{NTasks: 6})
			Expect(err).NotTo(HaveOccurred())
			return &supervisor{cfg: config.Default(), opt: &options{bindMode: mode, command: []string{"true"}}, lay: l, plan: p}
		}

		It("carries the step's binding mode and the job's slots on that host", func() {
			s := sup(cpubind.ModeAuto)
			first, second := s.stepSpec(nil, 0), s.stepSpec(nil, 1)
			Expect(first.CPUBind).To(Equal(cpubind.ModeAuto))
			Expect(first.NodeSlots).To(Equal(8))
			Expect(second.NodeSlots).To(Equal(5))
			Expect(first.HostSlots).To(Equal(8), "the host's capacity from the layout")
			Expect(second.HostSlots).To(Equal(16))
			Expect(sup(cpubind.ModeExplicit).stepSpec(nil, 1).CPUBind).To(Equal(cpubind.ModeExplicit))
		})

		It("plans each host's cpusets from its first allowed CPU, as positions", func() {
			spec := sup(cpubind.ModeAuto).stepSpec(nil, 1)
			var sets []string
			for _, r := range spec.Ranks {
				sets = append(sets, r.Cpuset)
			}
			Expect(sets).To(Equal([]string{"0-0", "1-1", "2-2", "3-3"}))
		})

		It("gives a hot-spare relaunch the spare's own slots", func() {
			s := sup(cpubind.ModeAuto)
			s.placeOnSpare(1, layout.Node{Index: 2, Host: "node003", Slots: 8, HostSlots: 12})
			spec := s.relaunchSpec(1)
			Expect(spec.NodeSlots).To(Equal(8))
			Expect(spec.HostSlots).To(Equal(12), "the spare's own capacity")
			Expect(spec.CPUBind).To(Equal(cpubind.ModeAuto))
		})
	})

	It("starts mpirun with the override over the caller's environment", func() {
		p := &plan.StepPlan{Nodes: []plan.StepNode{{Host: "node001"}}}
		bound := cpubind.Node{Allowed: []int{4, 5}, Online: 14}
		vars := append([]string{"SLURM_NTASKS=2"},
			cpubind.OpenMPIOverride(cpubind.ModeAuto, bound, []cpubind.Holding{{Slots: 2}}, func(string) string { return "" })...)
		env := launcherEnv([]string{"HOME=/home/u"}, vars, p)
		Expect(env).To(ContainElements(
			"OMPI_MCA_hwloc_base_binding_policy=none",
			"PRTE_MCA_hwloc_default_binding_policy=none",
			"HOME=/home/u"))
	})
})
