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

package srun_test

import (
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/cpubind"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// thisNode is what this test process's node offers: srun and its steppers
// inherit the same mask, so a pinned step's first task lands on Allowed[0].
func thisNode() cpubind.Node {
	n, err := cpubind.ThisNode()
	Expect(err).NotTo(HaveOccurred())
	Expect(n.Allowed).NotTo(BeEmpty())
	return n
}

// needs skips a spec the machine cannot run, or fails it where CI requires
// the CPU binding specs (SLURM_SHIM_REQUIRE_CPU_SPECS=1): a silent skip there
// would leave the rule untested.
func needs(ok bool, why string) {
	if ok {
		return
	}
	if os.Getenv("SLURM_SHIM_REQUIRE_CPU_SPECS") == "1" {
		Fail("CPU binding spec cannot run here: " + why)
	}
	Skip(why)
}

// The pinning rule end to end through the real srun and stepper (CS-2945):
// jobs that share a node must not pin to the same CPUs, and a job that Grid
// Engine binding gave some cores must stay on them.
var _ = Describe("srun CPU binding", func() {
	var node cpubind.Node

	BeforeEach(func() {
		needs(runtime.GOOS == "linux", "affinity is applied on Linux only")
		node = thisNode()
	})

	// oneNode is a one-node allocation granting slots slots, as Grid Engine would.
	oneNode := func(slots int) string {
		return writeAlloc([]layout.Node{{Index: 0, Host: "node001", Slots: slots, IsMaster: true}}, []int{slots})
	}
	const report = `echo "rank=$SLURM_PROCID allowed=$(grep Cpus_allowed_list /proc/self/status | cut -f2)"`

	// masks waits for srun and returns each rank's report line, sorted, and stderr.
	masks := func(sess *gexec.Session) ([]string, string) {
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := lines(sess)
		sort.Strings(out)
		return out, string(sess.Err.Contents())
	}
	taskset := func(cpus []int) []string {
		path, err := exec.LookPath("taskset")
		needs(err == nil, "taskset (util-linux) gives srun a Grid Engine-like mask")
		return []string{path, "-c", cpubind.FormatList(cpus)}
	}
	whole := func() string { return cpubind.FormatList(node.Allowed) }
	unrestricted := func() { needs(!node.Restricted(), "the test process's CPU mask is restricted") }

	It("leaves the task of a job that shares the node unpinned", func() {
		unrestricted()
		needs(len(node.Allowed) >= 2, "needs 2 CPUs, so 1 slot is part of the node")
		out, _ := masks(runSrun(oneNode(1), "-n", "1", "sh", "-c", report))
		Expect(out).To(Equal([]string{"rank=0 allowed=" + whole()}))
	})

	It("leaves 2 tasks unpinned for a job holding half the node's slots", func() {
		// The CI runners have 4 hardware threads on 2 cores and a slot per
		// thread: 2 slots equal the core count but are half the node, and a
		// second job can hold the other half (CS-2945 e2e, 2026-10-29).
		unrestricted()
		needs(len(node.Allowed) >= 4, "needs 4 CPUs")
		out, _ := masks(runSrun(oneNode(len(node.Allowed)/2), "-n", "2", "sh", "-c", report))
		Expect(out).To(Equal([]string{"rank=0 allowed=" + whole(), "rank=1 allowed=" + whole()}))
	})

	It("pins when Grid Engine says the job holds the host whole, with fewer slots than CPUs", func() {
		// A site with a slot per core, or an exclusive job: the layout's host
		// capacity decides, not the CPU count.
		unrestricted()
		needs(len(node.Allowed) >= 3, "needs 3 CPUs, so 2 slots are fewer")
		job := writeAlloc([]layout.Node{{Index: 0, Host: "node001", Slots: 2, HostSlots: 2, IsMaster: true}}, []int{2})
		out, _ := masks(runSrun(job, "-n", "2", "sh", "-c", report))
		Expect(out).To(Equal([]string{
			"rank=0 allowed=" + cpubind.FormatList(node.Allowed[0:1]),
			"rank=1 allowed=" + cpubind.FormatList(node.Allowed[1:2]),
		}))
	})

	It("leaves the tasks unpinned when Grid Engine says the host offers more slots", func() {
		unrestricted()
		needs(len(node.Allowed) >= 2, "needs 2 CPUs")
		n := len(node.Allowed)
		job := writeAlloc([]layout.Node{{Index: 0, Host: "node001", Slots: n, HostSlots: 2 * n, IsMaster: true}}, []int{n})
		out, _ := masks(runSrun(job, "-n", "2", "sh", "-c", report))
		Expect(out).To(Equal([]string{"rank=0 allowed=" + whole(), "rank=1 allowed=" + whole()}),
			"every CPU's slot, but a second queue on the host")
	})

	It("pins one CPU per task, on different cores, on a node the job owns", func() {
		unrestricted()
		needs(len(node.Allowed) >= 2, "needs 2 CPUs")
		out, _ := masks(runSrun(oneNode(len(node.Allowed)), "-n", "2", "sh", "-c", report))
		Expect(out).To(Equal([]string{
			"rank=0 allowed=" + cpubind.FormatList(node.Allowed[0:1]),
			"rank=1 allowed=" + cpubind.FormatList(node.Allowed[1:2]),
		}))
	})

	It("pins within the CPUs Grid Engine binding granted, not from CPU 0", func() {
		needs(len(node.Allowed) >= 3, "needs 3 CPUs")
		sorted := append([]int(nil), node.Allowed...)
		sort.Ints(sorted)
		granted := sorted[len(sorted)-2:] // never the first CPU
		out, stderr := masks(runSrunWrapped(oneNode(2), taskset(granted), nil, "-n", "2", "sh", "-c", report))
		Expect(out).To(HaveLen(2))
		Expect([]string{strings.TrimPrefix(out[0], "rank=0 allowed="), strings.TrimPrefix(out[1], "rank=1 allowed=")}).
			To(ConsistOf(cpubind.FormatList(granted[0:1]), cpubind.FormatList(granted[1:2])),
				"each task on its own granted CPU")
		Expect(stderr).NotTo(ContainSubstring("warning"))
	})

	It("runs the tasks unpinned within the grant, with a warning, when it is too small for them", func() {
		needs(len(node.Allowed) >= 2, "needs 2 CPUs")
		granted := node.Allowed[len(node.Allowed)-1:]
		out, stderr := masks(runSrunWrapped(oneNode(2), taskset(granted), nil, "-n", "2", "sh", "-c", report))
		only := cpubind.FormatList(granted)
		Expect(out).To(Equal([]string{"rank=0 allowed=" + only, "rank=1 allowed=" + only}))
		Expect(stderr).To(MatchRegexp(`too few for its tasks`))
	})

	It("pins on a shared node when --cpu-bind asks for it", func() {
		unrestricted()
		needs(len(node.Allowed) >= 2, "needs 2 CPUs, so 1 slot is part of the node")
		out, _ := masks(runSrun(oneNode(1), "--cpu-bind=cores", "-n", "1", "sh", "-c", report))
		Expect(out).To(Equal([]string{"rank=0 allowed=" + cpubind.FormatList(node.Allowed[0:1])}))
	})

	It("never pins under --cpu-bind=none, even within a grant", func() {
		needs(len(node.Allowed) >= 3, "needs 3 CPUs")
		granted := node.Allowed[len(node.Allowed)-2:]
		out, _ := masks(runSrunWrapped(oneNode(2), taskset(granted), nil, "--cpu-bind=none", "-n", "2", "sh", "-c", report))
		g := cpubind.FormatList(granted)
		Expect(out).To(Equal([]string{"rank=0 allowed=" + g, "rank=1 allowed=" + g}))
	})
})

var _ = Describe("srun CPU binding in the dry run", func() {
	dry := func(args ...string) string {
		tmp := writeAlloc([]layout.Node{{Index: 0, Host: "node001", Slots: 12, IsMaster: true}}, []int{12})
		sess := runSrunEnv(tmp, []string{"SLURM_SHIM_DRY_RUN=1"}, args...)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		return string(sess.Err.Contents())
	}

	It("plans cpus as positions in each node's allowed set and names the mode", func() {
		report := dry("-n", "11", "true")
		Expect(report).To(ContainSubstring("rank 1 on node001: local 1, cpu #1-1"))
		Expect(report).To(ContainSubstring("rank 10 on node001: local 10, cpu #10-10"))
		Expect(report).To(MatchRegexp(`cpu binding\s+auto\b`))
		Expect(strings.Count(report, "cpu binding")).To(Equal(1))
	})

	It("says per node whether the job holds the host", func() {
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, HostSlots: 4, IsMaster: true},
			{Index: 1, Host: "node002", Slots: 4, HostSlots: 8},
			{Index: 2, Host: "node003", Slots: 4},
		}, []int{4, 4, 4})
		sess := runSrunEnv(tmp, []string{"SLURM_SHIM_DRY_RUN=1"}, "-n", "12", "true")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		report := string(sess.Err.Contents())
		Expect(report).To(MatchRegexp(`slots node001\s+4 of 4: the job holds the host`))
		Expect(report).To(MatchRegexp(`slots node002\s+4 of 8: shared with other jobs`))
		Expect(report).To(MatchRegexp(`slots node003\s+4 held, host total unknown`))
	})

	It("names an explicit or disabled binding", func() {
		Expect(dry("--cpu-bind=cores", "-n", "2", "true")).To(MatchRegexp(`cpu binding\s+explicit\b`))
		none := dry("--cpu-bind=none", "-n", "2", "true")
		Expect(none).To(MatchRegexp(`cpu binding\s+none\b`))
		Expect(none).NotTo(ContainSubstring("cpu #"))
	})
})
