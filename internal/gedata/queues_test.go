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
