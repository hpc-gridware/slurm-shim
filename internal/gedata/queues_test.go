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

package gedata_test

import (
	"context"
	"errors"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// Real-shaped `qstat -f`: master busy (2/14 -> Used=2), worker1 full (14/14),
// worker2 disabled (states column "d"). Field order matters, not alignment.
const qstatFull = `queuename                      qtype resv/used/tot. load_avg arch          states
---------------------------------------------------------------------------------
all.q@ocs-master               BIP   0/2/14         0.90     lx-arm64
---------------------------------------------------------------------------------
all.q@ocs-worker1              BIP   0/14/14        0.90     lx-arm64
---------------------------------------------------------------------------------
all.q@ocs-worker2              BIP   0/0/14         0.90     lx-arm64      d
`

var _ = Describe("QueueInstances", func() {
	It("runs qstat -f and maps the parsed instances, populating states", func() {
		r := &fake.Runner{Responder: func(_ string, _ []string) fake.Response {
			return fake.Response{Stdout: []byte(qstatFull)}
		}}
		got, err := gedata.QueueInstances(context.Background(), r)
		Expect(err).NotTo(HaveOccurred())

		Expect(r.Calls[0].Name).To(Equal("qstat"))
		Expect(r.Calls[0].Args).To(Equal([]string{"-f"}))

		Expect(got).To(HaveLen(3))
		Expect(got[0]).To(Equal(gedata.QueueInstance{
			Name: "all.q@ocs-master", Queue: "all.q", Host: "ocs-master", Used: 2, Total: 14, States: "",
		}))
		Expect(got[1].Used).To(Equal(14))
		Expect(got[1].Total).To(Equal(14))
		// The disabled instance's states column must survive the parse.
		Expect(got[2].Host).To(Equal("ocs-worker2"))
		Expect(got[2].States).To(Equal("d"))
	})

	It("parses a real captured qstat -f (OCS 9.0.10)", func() {
		data, err := os.ReadFile("testdata/qstat-f-9.0.10.txt")
		Expect(err).NotTo(HaveOccurred())
		r := &fake.Runner{Responder: func(_ string, _ []string) fake.Response {
			return fake.Response{Stdout: data}
		}}
		got, err := gedata.QueueInstances(context.Background(), r)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(3))
		hosts := []string{got[0].Host, got[1].Host, got[2].Host}
		Expect(hosts).To(ConsistOf("ocs-master", "ocs-worker1", "ocs-worker2"))
		for _, q := range got {
			Expect(q.Queue).To(Equal("all.q"))
			Expect(q.Total).To(Equal(14))
		}
	})

	It("returns an error when qstat exits non-zero", func() {
		r := &fake.Runner{Responder: func(_ string, _ []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qmaster down")}
		}}
		_, err := gedata.QueueInstances(context.Background(), r)
		Expect(err).To(HaveOccurred())
	})

	It("propagates a runner spawn error", func() {
		r := &fake.Runner{Responder: func(_ string, _ []string) fake.Response {
			return fake.Response{Err: errors.New("exec: qstat not found")}
		}}
		_, err := gedata.QueueInstances(context.Background(), r)
		Expect(err).To(HaveOccurred())
	})
})

// The slots a host offers any job (CS-2945): a job holding all of them has the
// host to itself, whatever its CPU topology.
var _ = Describe("HostSlotCapacity", func() {
	// Two queues on ocs-worker1 (14 + 14), one on ocs-master; worker1's own
	// limit is 14 (complex_values slots=14, the captured qconf -se).
	const twoQueues = `queuename                      qtype resv/used/tot. load_avg arch          states
---------------------------------------------------------------------------------
all.q@ocs-master               BIP   0/2/14         0.90     lx-arm64
---------------------------------------------------------------------------------
all.q@ocs-worker1.example      BIP   0/2/14         0.90     lx-arm64
---------------------------------------------------------------------------------
gpu.q@ocs-worker1.example      BIP   0/0/14         0.90     lx-arm64
`
	exechost := func() []byte {
		data, err := os.ReadFile("testdata/qconf_se_gpu_9.1.6.txt")
		Expect(err).NotTo(HaveOccurred())
		return data
	}

	It("sums a host's queue instances and caps them at its own slots limit", func() {
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			switch {
			case name == "qstat":
				return fake.Response{Stdout: []byte(twoQueues)}
			case name == "qconf" && args[1] == "ocs-worker1.example":
				return fake.Response{Stdout: exechost()}
			default:
				return fake.Response{Exit: 1, Stderr: []byte("no limit")}
			}
		}}
		got, err := gedata.HostSlotCapacity(context.Background(), r, []string{"ocs-master", "ocs-worker1.example"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(map[string]int{"ocs-master": 14, "ocs-worker1": 14}),
			"28 queue slots, but the host lets only 14 run; names are short")

		Expect(r.Calls[0].Args).To(Equal([]string{"-f", "-q", "*@ocs-master,*@ocs-worker1.example"}),
			"only the job's hosts, not the whole cluster")
	})

	It("counts every queue on a host without a slots limit", func() {
		r := &fake.Runner{Responder: func(name string, _ []string) fake.Response {
			if name == "qstat" {
				return fake.Response{Stdout: []byte(twoQueues)}
			}
			return fake.Response{Stdout: []byte("hostname ocs-worker1\ncomplex_values NONE\n")}
		}}
		got, err := gedata.HostSlotCapacity(context.Background(), r, []string{"ocs-worker1.example"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got["ocs-worker1"]).To(Equal(28))
	})

	It("leaves a host without queue instances unknown", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte(twoQueues)}
		}}
		got, err := gedata.HostSlotCapacity(context.Background(), r, []string{"node999"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(HaveKey("node999"))
	})

	It("reports a failing qstat", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qmaster down")}
		}}
		_, err := gedata.HostSlotCapacity(context.Background(), r, []string{"ocs-master"})
		Expect(err).To(MatchError(ContainSubstring("qmaster down")))
	})
})
