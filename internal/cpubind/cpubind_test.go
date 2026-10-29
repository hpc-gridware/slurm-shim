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

package cpubind_test

import (
	"runtime"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/cpubind"
)

// cpus builds a CPU list from its text form, for compact table entries.
func cpus(list string) []int {
	out, err := cpubind.ParseList(list)
	Expect(err).NotTo(HaveOccurred())
	return out
}

// node is a node with 14 CPUs online whose job may use allowed.
func node(allowed string) cpubind.Node {
	a := cpus(allowed)
	return cpubind.Node{Allowed: a, Online: 14}
}

// slots is a holding whose host capacity is unknown (the CPU-count fallback).
func slots(n int) cpubind.Holding { return cpubind.Holding{Slots: n} }

// held is holdings with unknown host capacity, one per node.
func held(n ...int) []cpubind.Holding {
	out := make([]cpubind.Holding, len(n))
	for i, s := range n {
		out[i] = slots(s)
	}
	return out
}

var _ = Describe("CPU binding", func() {
	DescribeTable("maps --cpu-bind to a mode",
		func(spec, want string) { Expect(cpubind.ModeFor(spec)).To(Equal(want)) },
		Entry("unset: automatic", "", cpubind.ModeAuto),
		Entry("none", "none", cpubind.ModeNone),
		Entry("none with a qualifier", "verbose,none", cpubind.ModeNone),
		Entry("verbose alone selects no type", "verbose", cpubind.ModeAuto),
		Entry("cores", "cores", cpubind.ModeExplicit),
		Entry("threads", "threads", cpubind.ModeExplicit),
		Entry("rank", "rank", cpubind.ModeExplicit),
		Entry("map_cpu", "map_cpu:0,2", cpubind.ModeExplicit),
		Entry("qualified type", "verbose,cores", cpubind.ModeExplicit),
	)

	// The rule that keeps jobs sharing a node off each other's CPUs (CS-2945).
	DescribeTable("decides whether a node's tasks are pinned",
		func(mode string, n cpubind.Node, h cpubind.Holding, want bool) {
			Expect(cpubind.ShouldPin(mode, n, h)).To(Equal(want))
		},
		Entry("Grid Engine binding narrowed the mask: pin within it", cpubind.ModeAuto, node("4-7"), slots(2), true),
		Entry("whole node allowed, a slot per CPU: owns the node", cpubind.ModeAuto, node("0-13"), slots(14), true),
		Entry("whole node allowed, more slots than CPUs: owns the node", cpubind.ModeAuto, node("0-13"), slots(28), true),
		Entry("whole node allowed, job holds part of it: shared, unpinned", cpubind.ModeAuto, node("0-13"), slots(2), false),
		// The CI runners: 4 hardware threads on 2 cores, a slot per thread. A job
		// holding 2 slots holds half the node; counting cores made two such jobs
		// "owners" that pinned the same CPUs.
		Entry("SMT node, half its slots: shared, even though they equal its cores",
			cpubind.ModeAuto, cpubind.Node{Allowed: cpus("0-3"), Online: 4}, slots(2), false),
		Entry("SMT node, a slot per thread: owns the node",
			cpubind.ModeAuto, cpubind.Node{Allowed: cpus("0-3"), Online: 4}, slots(4), true),
		Entry("SMT node, a slot per core (8 of 16 threads): unpinned, never colliding",
			cpubind.ModeAuto, cpubind.Node{Allowed: cpus("0-15"), Online: 16}, slots(8), false),
		Entry("online unknown: an unrestricted mask, judged by ownership",
			cpubind.ModeAuto, cpubind.Node{Allowed: cpus("0-3")}, slots(2), false),
		Entry("online unknown, job holds every CPU: owned",
			cpubind.ModeAuto, cpubind.Node{Allowed: cpus("0-3")}, slots(4), true),
		Entry("no slots sent (an older srun): shared, unpinned", cpubind.ModeAuto, node("0-13"), slots(0), false),
		// Grid Engine knows the host's slots: ownership no longer depends on how
		// the site counts SMT threads or on other queues on the host.
		Entry("slot per core, the job holds all 8 of 16 threads: Grid Engine says owned",
			cpubind.ModeAuto, cpubind.Node{Allowed: cpus("0-15"), Online: 16}, cpubind.Holding{Slots: 8, HostSlots: 8}, true),
		Entry("slot per thread, 2 of 4 (the CI runners): shared",
			cpubind.ModeAuto, cpubind.Node{Allowed: cpus("0-3"), Online: 4}, cpubind.Holding{Slots: 2, HostSlots: 4}, false),
		Entry("two queues on the host (14 + 14), the job holds 14: shared",
			cpubind.ModeAuto, node("0-13"), cpubind.Holding{Slots: 14, HostSlots: 28}, false),
		Entry("exclusive job with 2 slots (host slots = its slots): owned",
			cpubind.ModeAuto, node("0-13"), cpubind.Holding{Slots: 2, HostSlots: 2}, true),
		Entry("capacity known wins over the CPU count",
			cpubind.ModeAuto, node("0-13"), cpubind.Holding{Slots: 14, HostSlots: 20}, false),
		Entry("--cpu-bind=none never pins, even when bound", cpubind.ModeNone, node("4-7"), slots(2), false),
		Entry("--cpu-bind=none never pins an owned node", cpubind.ModeNone, node("0-13"), slots(14), false),
		Entry("explicit binding pins on a shared node", cpubind.ModeExplicit, node("0-13"), slots(2), true),
		Entry("no CPU facts (non-Linux): never", cpubind.ModeExplicit, cpubind.Node{}, slots(14), false),
	)

	It("names a mask narrower than the node restricted, and an unknown node not", func() {
		Expect(node("4-7").Restricted()).To(BeTrue())
		Expect(node("0-13").Restricted()).To(BeFalse())
		Expect(cpubind.Node{Allowed: cpus("0-3")}.Restricted()).To(BeFalse())
	})

	// OpenMPI binds once, in mpirun, for every node: the override must hold for
	// all nodes, and stay off where OpenMPI's own per-core binding is safe (it
	// is faster than unpinned ranks).
	DescribeTable("tells OpenMPI not to bind where it would leave the job's CPUs",
		func(mode string, master cpubind.Node, holdings []cpubind.Holding, env map[string]string, wantNone bool) {
			got := cpubind.OpenMPIOverride(mode, master, holdings, func(k string) string { return env[k] })
			if wantNone {
				Expect(got).To(ConsistOf(
					"OMPI_MCA_hwloc_base_binding_policy=none",
					"PRTE_MCA_hwloc_default_binding_policy=none"))
			} else {
				Expect(got).To(BeEmpty())
			}
		},
		Entry("Grid Engine binding: OpenMPI would bind outside the grant", cpubind.ModeAuto, node("4-5"), held(2, 2), nil, true),
		Entry("explicit binding under a grant: still outside it", cpubind.ModeExplicit, node("4-5"), held(2), nil, true),
		Entry("shared node, no binding: cores 0, 1 may be another job's", cpubind.ModeAuto, node("0-13"), held(2), nil, true),
		Entry("first node owned, a second shared: that node would collide", cpubind.ModeAuto, node("0-13"), held(14, 2), nil, true),
		Entry("--cpu-bind=none", cpubind.ModeNone, node("0-13"), held(14), nil, true),
		Entry("every node owned: OpenMPI's per-core binding", cpubind.ModeAuto, node("0-13"), held(14, 14, 14), nil, false),
		Entry("SMT nodes, half their slots: shared (the CI runners)", cpubind.ModeAuto,
			cpubind.Node{Allowed: cpus("0-3"), Online: 4}, held(2, 2), nil, true),
		Entry("SMT nodes, a slot per thread: owned", cpubind.ModeAuto,
			cpubind.Node{Allowed: cpus("0-3"), Online: 4}, held(4, 4), nil, false),
		Entry("every node owned by Grid Engine's count, slot per core", cpubind.ModeAuto,
			cpubind.Node{Allowed: cpus("0-15"), Online: 16},
			[]cpubind.Holding{{Slots: 8, HostSlots: 8}, {Slots: 8, HostSlots: 8}}, nil, false),
		Entry("a node with a second queue: shared", cpubind.ModeAuto, node("0-13"),
			[]cpubind.Holding{{Slots: 14, HostSlots: 14}, {Slots: 14, HostSlots: 28}}, nil, true),
		Entry("explicit binding on shared nodes", cpubind.ModeExplicit, node("0-13"), held(2, 2), nil, false),
		Entry("the user chose OpenMPI 4's policy", cpubind.ModeAuto, node("4-5"), held(2),
			map[string]string{"OMPI_MCA_hwloc_base_binding_policy": "core"}, false),
		Entry("the user chose OpenMPI 5's policy", cpubind.ModeAuto, node("0-13"), held(2),
			map[string]string{"PRTE_MCA_hwloc_default_binding_policy": "core"}, false),
		Entry("no CPU facts (non-Linux): as before", cpubind.ModeAuto, cpubind.Node{}, held(2), nil, false),
	)

	Describe("resolving a node's tasks", func() {
		It("places each task on its own CPU within a bound mask, not from CPU 0", func() {
			got, warn := cpubind.Resolve(cpubind.ModeAuto, []string{"0-0", "1-1"}, node("2-3"), slots(2))
			Expect(got).To(Equal([]string{"2", "3"}))
			Expect(warn).To(BeEmpty())
		})

		It("maps positions over gaps in the mask", func() {
			got, _ := cpubind.Resolve(cpubind.ModeExplicit, []string{"1-2"}, node("1,3,5,9"), slots(2))
			Expect(got).To(Equal([]string{"3,5"}))
		})

		It("leaves every task unpinned on a shared node", func() {
			got, warn := cpubind.Resolve(cpubind.ModeAuto, []string{"0-0", "1-1"}, node("0-13"), slots(2))
			Expect(got).To(Equal([]string{"", ""}))
			Expect(warn).To(BeEmpty())
		})

		It("keeps pinning from the first CPU on a node the job owns", func() {
			got, _ := cpubind.Resolve(cpubind.ModeAuto, []string{"0-1", "2-3"}, node("0-13"), slots(14))
			Expect(got).To(Equal([]string{"0-1", "2-3"}))
		})

		It("pins no task, and says why, when the mask is too small for them all", func() {
			got, warn := cpubind.Resolve(cpubind.ModeAuto, []string{"0-1", "2-3"}, node("4-6"), slots(2))
			Expect(got).To(Equal([]string{"", ""}), "all or nothing: no half-pinned node")
			Expect(warn).To(ContainSubstring("3 CPU(s) here (4-6), too few"))

			got, _ = cpubind.Resolve(cpubind.ModeExplicit, []string{"0,5"}, node("0-3"), slots(4))
			Expect(got).To(Equal([]string{""}), "one position beyond the mask is enough")
		})

		It("keeps a task planned without a cpuset unpinned, and an unparsable one unpinned", func() {
			got, _ := cpubind.Resolve(cpubind.ModeExplicit, []string{"", "0-0"}, node("4-7"), slots(2))
			Expect(got).To(Equal([]string{"", "4"}))
			got, warn := cpubind.Resolve(cpubind.ModeExplicit, []string{"x"}, node("4-7"), slots(2))
			Expect(got).To(Equal([]string{""}))
			Expect(warn).NotTo(BeEmpty())
		})

		It("pins nothing under --cpu-bind=none whatever was planned", func() {
			got, warn := cpubind.Resolve(cpubind.ModeNone, []string{"0-0", "1-1"}, node("4-7"), slots(2))
			Expect(got).To(Equal([]string{"", ""}))
			Expect(warn).To(BeEmpty())
		})
	})

	Describe("ordering a node's CPUs one thread per core first", func() {
		// An SMT node numbered the Linux way: core i has threads i and i+4.
		siblings := map[int]int{0: 0, 1: 1, 2: 2, 3: 3, 4: 0, 5: 1, 6: 2, 7: 3}

		It("puts consecutive tasks on different cores before two share one", func() {
			Expect(cpubind.CoreFirst(cpus("0-7"), siblings)).To(Equal([]int{0, 1, 2, 3, 4, 5, 6, 7}))

			// Grid Engine bound two cores (-bunit C): both threads of each.
			ordered := cpubind.CoreFirst(cpus("2,3,6,7"), siblings)
			Expect(ordered).To(Equal([]int{2, 3, 6, 7}))
			got, _ := cpubind.Resolve(cpubind.ModeAuto, []string{"0-0", "1-1"},
				cpubind.Node{Allowed: ordered, Online: 8}, slots(2))
			Expect(got).To(Equal([]string{"2", "3"}), "two cores, not the two threads of core 2")
		})

		It("reorders a node that numbers a core's threads next to each other", func() {
			// Core i has threads 2i and 2i+1.
			adjacent := map[int]int{0: 0, 1: 0, 2: 2, 3: 2}
			Expect(cpubind.CoreFirst(cpus("0-3"), adjacent)).To(Equal([]int{0, 2, 1, 3}))
		})

		It("treats CPUs without topology as cores of their own", func() {
			Expect(cpubind.CoreFirst(cpus("0-3"), nil)).To(Equal([]int{0, 1, 2, 3}))
		})
	})

	DescribeTable("parses CPU lists as Linux prints them",
		func(in string, want []int) {
			got, err := cpubind.ParseList(in)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("range", "0-3", []int{0, 1, 2, 3}),
		Entry("range plus single", "0-1,4", []int{0, 1, 4}),
		Entry("single", "5", []int{5}),
		Entry("unsorted and repeated", "5,0-1,1", []int{0, 1, 5}),
		Entry("spaces tolerated", " 0-2 ", []int{0, 1, 2}),
	)

	DescribeTable("rejects malformed CPU lists",
		func(in string) {
			_, err := cpubind.ParseList(in)
			Expect(err).To(HaveOccurred())
		},
		Entry("descending range", "3-1"),
		Entry("not a number", "a-b"),
		Entry("empty", ""),
		Entry("negative", "-1"),
	)

	It("reads this node's mask, cores and online CPUs", func() {
		if runtime.GOOS != "linux" {
			n, err := cpubind.ThisNode()
			Expect(err).NotTo(HaveOccurred())
			Expect(n.Allowed).To(BeEmpty(), "no CPU facts off Linux: never pinned")
			return
		}
		n, err := cpubind.ThisNode()
		Expect(err).NotTo(HaveOccurred())
		Expect(n.Allowed).NotTo(BeEmpty())
		Expect(n.Online).To(BeNumerically(">=", len(n.Allowed)))
	})

	It("formats CPU lists compactly", func() {
		Expect(cpubind.FormatList([]int{0, 1, 2, 3, 5})).To(Equal("0-3,5"))
		Expect(cpubind.FormatList([]int{7, 6})).To(Equal("6-7"))
		Expect(cpubind.FormatList(nil)).To(Equal(""))
	})
})
