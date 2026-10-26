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
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

var _ = Describe("srun starting an MPI launcher's daemons", func() {
	threeNodes := func() string {
		return writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true},
			{Index: 1, Host: "node002", Slots: 4},
			{Index: 2, Host: "node003", Slots: 4},
		}, []int{4, 4, 4})
	}
	// Each daemon reports what PRRTE's and ORTE's ess/slurm read to name it.
	const report = `echo "$SLURMD_NODENAME procid=$SLURM_PROCID nodeid=$SLURM_NODEID"`

	sortedLines := func(sess *gexec.Session) []string {
		out := lines(sess)
		sort.Strings(out)
		return out
	}

	It("puts PRRTE 3.0's daemons on the nodes other than the master when no node list is given", func() {
		sess := runSrun(threeNodes(), "--external-launcher", "--ntasks-per-node=1", "--kill-on-bad-exit",
			"--mpi=none", "--cpu-bind=none", "--ntasks=2", "sh", "-c", report)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(sortedLines(sess)).To(Equal([]string{
			"node002 procid=0 nodeid=0",
			"node003 procid=1 nodeid=1",
		}))
		Expect(string(sess.Err.Contents())).NotTo(ContainSubstring("warning"))
	})

	It("numbers PRRTE master's daemons in the order of its node file", func() {
		tmp := threeNodes()
		nodes := filepath.Join(tmp, "nodes")
		Expect(os.WriteFile(nodes, []byte("node003\nnode002\n"), 0o600)).To(Succeed())
		sess := runSrun(tmp, "--external-launcher", "--ntasks-per-node=1", "--no-kill", "--kill-on-bad-exit=0",
			"--mpi=none", "--cpu-bind=none", "--jobid=4711", "--distribution=arbitrary",
			"--nodelist="+nodes, "--ntasks=2", "sh", "-c", report)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(sortedLines(sess)).To(Equal([]string{
			"node002 procid=1 nodeid=1",
			"node003 procid=0 nodeid=0",
		}))
		Expect(string(sess.Err.Contents())).NotTo(ContainSubstring("warning"))
	})

	It("lets the other daemons run on when one exits non-zero under --kill-on-bad-exit=0", func() {
		// PRRTE master passes --kill-on-bad-exit=0: it, not srun, decides what
		// a lost daemon means.
		sess := runSrun(threeNodes(), "--external-launcher", "--kill-on-bad-exit=0", "--ntasks-per-node=1",
			"--ntasks=2", "sh", "-c", `if [ "$SLURM_PROCID" = 0 ]; then exit 7; fi; sleep 2; echo survivor`)
		Eventually(sess, "30s").Should(gexec.Exit(7))
		Expect(lines(sess)).To(ContainElement("survivor"))
	})

	It("refuses --jobid for another job before starting anything", func() {
		sess := runSrun(threeNodes(), "--jobid=99", "-n", "1", "true")
		Eventually(sess, "30s").Should(gexec.Exit(1))
		Expect(string(sess.Err.Contents())).To(ContainSubstring("only inside the job srun runs in"))
	})

	It("plans no cpuset for an external launcher or --cpu-bind=none, and keeps one otherwise", func() {
		dry := func(args ...string) string {
			sess := runSrunEnv(threeNodes(), []string{"SLURM_SHIM_DRY_RUN=1"}, args...)
			Eventually(sess, "30s").Should(gexec.Exit(0))
			report := string(sess.Out.Contents()) + string(sess.Err.Contents())
			Expect(report).To(ContainSubstring("rank 0 on"), "the report lists the ranks")
			return report
		}
		Expect(dry("--external-launcher", "--ntasks-per-node=1", "--ntasks=2", "prted")).NotTo(ContainSubstring("cpuset"))
		Expect(dry("--cpu-bind=none", "-N", "3", "hostname")).NotTo(ContainSubstring("cpuset"))
		Expect(dry("-N", "3", "hostname")).To(ContainSubstring("cpuset 0-0"))
	})

	It("leaves a daemon's affinity alone, so the ranks it forks are not all on one CPU", func() {
		if runtime.GOOS != "linux" || runtime.NumCPU() < 2 {
			Skip("affinity is applied on Linux only, and needs more than one CPU to show")
		}
		allowed := `grep Cpus_allowed_list /proc/self/status | cut -f2`
		pinned := runSrun(threeNodes(), "-N", "1", "sh", "-c", allowed)
		Eventually(pinned, "30s").Should(gexec.Exit(0))
		Expect(strings.TrimSpace(string(pinned.Out.Contents()))).To(Equal("0"))

		free := runSrun(threeNodes(), "--external-launcher", "--ntasks-per-node=1", "--ntasks=1", "sh", "-c", allowed)
		Eventually(free, "30s").Should(gexec.Exit(0))
		Expect(strings.TrimSpace(string(free.Out.Contents()))).To(MatchRegexp(`^0-[0-9]+`), "all CPUs, not one")
	})
})
