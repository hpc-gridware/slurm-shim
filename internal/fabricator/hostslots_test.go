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

package fabricator_test

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// The fabricator records the slots each host offers any job (CS-2945), so a
// step can tell a node the job holds whole from one it shares.
var _ = Describe("host slot capacity in the layout", func() {
	hostfile := "node001 4 all.q@node001 UNDEFINED\n" +
		"node002 2 all.q@node002 UNDEFINED\n" +
		"node003 4 all.q@node003 UNDEFINED\n"
	// node001 and node003 offer 4 slots, node002 has a second queue (2 + 4).
	const qstatF = `queuename                      qtype resv/used/tot. load_avg arch          states
---------------------------------------------------------------------------------
all.q@node001                  BIP   0/4/4          0.10     lx-amd64
---------------------------------------------------------------------------------
all.q@node002                  BIP   0/2/2          0.10     lx-amd64
---------------------------------------------------------------------------------
gpu.q@node002                  BIP   0/0/4          0.10     lx-amd64
---------------------------------------------------------------------------------
all.q@node003                  BIP   0/4/4          0.10     lx-amd64
`
	grid := func(qstatJ []byte) *fake.Runner {
		return &fake.Runner{Responder: func(name string, args []string) fake.Response {
			switch {
			case name == "qstat" && args[0] == "-f":
				return fake.Response{Stdout: []byte(qstatF)}
			case name == "qstat" && qstatJ != nil:
				return fake.Response{Stdout: qstatJ}
			default:
				return fake.Response{Exit: 1, Stderr: []byte("not set")}
			}
		}}
	}

	It("records each node's capacity, and a spare's for the node it replaces", func() {
		res := hostfileFab(hostfile, map[string]string{"SLURM_SHIM_SPARES": "1"}, grid(nil), config.Default())
		Expect(res.Layout.Nodes[0].HostSlots).To(Equal(4), "the job holds node001 whole")
		Expect(res.Layout.Nodes[1].HostSlots).To(Equal(6), "node002 has a second queue")
		Expect(res.Layout.Spares[0].HostSlots).To(Equal(4))
	})

	It("records a job that asked for its hosts exclusively as holding each whole", func() {
		qstatJ, err := os.ReadFile("../gedata/testdata/qstat_j_mem.xml")
		Expect(err).NotTo(HaveOccurred())
		cfg := config.Default()
		cfg.ExclusiveComplex = "h_vmem" // stands in for the site's exclusive complex
		res := hostfileFab(hostfile, nil, grid(qstatJ), cfg)
		for _, n := range res.Layout.Nodes {
			Expect(n.HostSlots).To(Equal(n.Slots), n.Host)
		}
	})

	It("leaves the capacity unknown, with one warning, when qstat fails", func() {
		down := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qmaster down")}
		}}
		res := hostfileFab(hostfile, nil, down, config.Default())
		for _, n := range res.Layout.Nodes {
			Expect(n.HostSlots).To(BeZero())
		}
		Expect(res.Warnings).To(ContainElement(ContainSubstring("cannot read the slots of the job's hosts")))
	})

	It("asks nothing without a runner (wrapper mode, sbatch prediction)", func() {
		res := hostfileFab(hostfile, nil, nil, config.Default())
		Expect(res.Layout.Nodes[0].HostSlots).To(BeZero())
	})
})
