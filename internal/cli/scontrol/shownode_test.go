package scontrol

import (
	"bytes"
	"io"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// clusterResponse replays the OCS 9.1.6 host and queue captures.
func clusterResponse(name string, args []string) fake.Response {
	switch name + " " + strings.Join(args, " ") {
	case "qhost -F":
		return fake.Response{Stdout: geFixture("qhost_F_9.1.6.txt")}
	case "qstat -f":
		return fake.Response{Stdout: geFixture("qstat_f_9.1.6.txt")}
	case "qconf -sc":
		return fake.Response{Stdout: geFixture("qconf_sc_9.1.6.txt")}
	case "qconf -se ocs-worker1", "qconf -se ocs-worker2":
		return fake.Response{Stdout: geFixture("qconf_se_gpu_9.1.6.txt")}
	case "qconf -sq all.q":
		return fake.Response{Stdout: geFixture("qconf_sq_9.1.6.txt")}
	}
	return fake.Response{Exit: 1, Stderr: []byte("unexpected call")}
}

func runShow(args ...string) (string, string, int) {
	var out, errOut bytes.Buffer
	code := run(&fake.Runner{Responder: clusterResponse}, testConfig(), append([]string{"show"}, args...), &out, &errOut)
	return out.String(), errOut.String(), code
}

var _ = Describe("scontrol show node", func() {
	It("renders a GPU node with its features, total GPUs and slot use", func() {
		out, errOut, code := runShow("node", "ocs-worker1")
		Expect(code).To(Equal(0))
		Expect(errOut).To(BeEmpty())
		Expect(out).To(Equal(`NodeName=ocs-worker1 Arch=lx-arm64
   CPUAlloc=4 CPUTot=14 CPULoad=0.92
   AvailableFeatures=e2e_avx
   ActiveFeatures=e2e_avx
   Gres=gpu:2
   RealMemory=12015
   State=MIXED
   Partitions=batch,smp

`))
	})

	It("lists every host, one record each, and one line each with -o", func() {
		out, _, code := runShow("-o", "node")
		Expect(code).To(Equal(0))
		lines := strings.Split(strings.TrimSpace(out), "\n")
		Expect(lines).To(HaveLen(3))
		Expect(lines[0]).To(HavePrefix("NodeName=ocs-master "))
		Expect(lines[0]).To(ContainSubstring("Gres=(null)"))
	})

	It("errors on an unknown node", func() {
		_, errOut, code := runShow("node", "nope")
		Expect(code).To(Equal(1))
		Expect(errOut).To(Equal("scontrol: error: Node nope not found\n"))
	})

	DescribeTable("folds queue instance states into a node state",
		func(node gedata.Node, instances []gedata.QueueInstance, want string) {
			alloc := 0
			for _, qi := range instances {
				alloc += qi.Used
			}
			Expect(nodeState(node, instances, alloc)).To(Equal(want))
		},
		Entry("idle", gedata.Node{Arch: "lx-amd64", CPUs: 4}, []gedata.QueueInstance{{Total: 4}}, "IDLE"),
		Entry("mixed", gedata.Node{Arch: "lx-amd64", CPUs: 4}, []gedata.QueueInstance{{Used: 1, Total: 4}}, "MIXED"),
		Entry("allocated", gedata.Node{Arch: "lx-amd64", CPUs: 4}, []gedata.QueueInstance{{Used: 4, Total: 4}}, "ALLOCATED"),
		Entry("no load values", gedata.Node{Arch: "-", CPUs: 4}, []gedata.QueueInstance{{Total: 4}}, "DOWN"),
		Entry("unreachable instance", gedata.Node{Arch: "lx-amd64"}, []gedata.QueueInstance{{Total: 4, States: "au"}}, "DOWN"),
		Entry("disabled instance", gedata.Node{Arch: "lx-amd64"}, []gedata.QueueInstance{{Used: 1, Total: 4, States: "d"}}, "DRAINED"),
		Entry("one of two queues disabled", gedata.Node{Arch: "lx-amd64", CPUs: 8},
			[]gedata.QueueInstance{{Total: 4, States: "d"}, {Total: 4}}, "IDLE"),
		Entry("in no queue", gedata.Node{Arch: "lx-amd64"}, nil, "DOWN"),
	)
})

var _ = Describe("scontrol show partition", func() {
	It("renders every configured partition from its queue", func() {
		out, errOut, code := runShow("partition")
		Expect(code).To(Equal(0))
		Expect(errOut).To(BeEmpty())
		Expect(out).To(Equal(`PartitionName=batch
   Default=YES
   MaxTime=UNLIMITED
   Nodes=ocs-master,ocs-worker[1-2]
   State=UP TotalCPUs=42 TotalNodes=3

PartitionName=smp
   Default=NO
   MaxTime=UNLIMITED
   Nodes=ocs-master,ocs-worker[1-2]
   State=UP TotalCPUs=42 TotalNodes=3

`))
	})

	It("reads each queue once, however many partitions share it", func() {
		r := &fake.Runner{Responder: clusterResponse}
		Expect(run(r, testConfig(), []string{"show", "partition"}, io.Discard, io.Discard)).To(Equal(0))
		sq := 0
		for _, c := range r.Calls {
			if c.Name == "qconf" && c.Args[0] == "-sq" {
				sq++
			}
		}
		Expect(sq).To(Equal(1))
	})

	It("shows a queue h_rt as MaxTime and an all-disabled queue as DRAIN", func() {
		rec := partitionRecord("gpu", "gpu.q", false, "48:00:00",
			[]gedata.QueueInstance{{Queue: "gpu.q", Host: "n1", Total: 8, States: "d"}})
		var out bytes.Buffer
		printRecord(&out, rec, true)
		Expect(out.String()).To(Equal("PartitionName=gpu Default=NO MaxTime=2-00:00:00 Nodes=n1 State=DRAIN TotalCPUs=8 TotalNodes=1\n"))
	})

	It("errors on an unknown partition and without a config", func() {
		_, errOut, code := runShow("partition", "nope")
		Expect(code).To(Equal(1))
		Expect(errOut).To(Equal("scontrol: error: Partition nope not found\n"))

		var errBuf bytes.Buffer
		Expect(run(&fake.Runner{}, nil, []string{"show", "partition"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("config could not be loaded"))
	})
})
