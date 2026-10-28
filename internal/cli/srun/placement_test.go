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
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

var _ = Describe("srun spreading -n over the step nodes", func() {
	// Three nodes with two tasks each, as sbatch -N 3 --ntasks-per-node=2.
	threeByTwo := func() string {
		return writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 2, IsMaster: true},
			{Index: 1, Host: "node002", Slots: 2},
			{Index: 2, Host: "node003", Slots: 2},
		}, []int{2, 2, 2})
	}
	const report = `echo "$SLURMD_NODENAME rank=$SLURM_PROCID nodeid=$SLURM_NODEID ` +
		`nodes=$SLURM_STEP_NUM_NODES list=$SLURM_STEP_NODELIST tpn=$SLURM_STEP_TASKS_PER_NODE"`

	run := func(args ...string) ([]string, string) {
		sess := runSrun(threeByTwo(), append(args, "sh", "-c", report)...)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := lines(sess)
		sort.Strings(out)
		return out, string(sess.Err.Contents())
	}

	It("runs -N 2 -n 2 one task per node, with matching step variables", func() {
		out, stderr := run("-N", "2", "-n", "2")
		Expect(out).To(Equal([]string{
			"node001 rank=0 nodeid=0 nodes=2 list=node[001-002] tpn=1(x2)",
			"node002 rank=1 nodeid=1 nodes=2 list=node[001-002] tpn=1(x2)",
		}))
		Expect(stderr).NotTo(ContainSubstring("Warning"))
	})

	It("starts no stepper on a node without a task when -n is below the node count", func() {
		out, stderr := run("-n", "1")
		Expect(out).To(Equal([]string{"node001 rank=0 nodeid=0 nodes=1 list=node001 tpn=1"}))
		Expect(stderr).To(ContainSubstring("can't run 1 processes on 3 nodes, setting nnodes to 1"))
	})
})
