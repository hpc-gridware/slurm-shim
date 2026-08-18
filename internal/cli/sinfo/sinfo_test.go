package sinfo

import (
	"bytes"
	"io"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func TestSinfo(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Sinfo Suite")
}

// fakeQstat returns a Runner that replays a canned `qstat -f` document.
func fakeQstat(out string) *fake.Runner {
	return &fake.Runner{Responder: func(_ string, _ []string) fake.Response {
		return fake.Response{Stdout: []byte(out)}
	}}
}

// Two idle nodes on all.q.
const twoNodeIdle = `queuename                      qtype resv/used/tot. load_avg arch          states
---------------------------------------------------------------------------------
all.q@node1                    BIP   0/0/8          0.10     lx-arm64
---------------------------------------------------------------------------------
all.q@node2                    BIP   0/0/8          0.10     lx-arm64
`

// Three nodes, three states: node1 idle (0/0/8), node2 allocated (8/8), node3
// disabled -> states column "au" -> down.
const threeStates = `queuename                      qtype resv/used/tot. load_avg arch          states
---------------------------------------------------------------------------------
all.q@node1                    BIP   0/0/8          0.10     lx-arm64
---------------------------------------------------------------------------------
all.q@node2                    BIP   0/8/8          0.10     lx-arm64
---------------------------------------------------------------------------------
all.q@node3                    BIP   0/0/8          0.10     lx-arm64      au
`

var _ = Describe("sinfo [REQ-SIN-001]", func() {
	It("prints a partition table derived from config, sorted", func() {
		cfg := config.Default()
		cfg.Partitions = map[string]config.Partition{
			"gpu":   {Queue: "gpu.q", PE: "gpu.pe"},
			"batch": {Queue: "all.q", PE: "smp.pe"},
		}
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), cfg, nil, &out, io.Discard)).To(Equal(0))
		lines := out.String()
		Expect(lines).To(HavePrefix("PARTITION AVAIL TIMELIMIT NODES STATE NODELIST\n"))
		Expect(lines).To(ContainSubstring("batch up infinite"))
		Expect(lines).To(ContainSubstring("gpu up infinite"))
		Expect(bytes_IndexBatchBeforeGpu(out.Bytes())).To(BeTrue())
	})

	It("shows live node count, state, and compressed nodelist for the partition's queue", func() {
		cfg := config.Default()
		cfg.Partitions = map[string]config.Partition{"batch": {Queue: "all.q"}}
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), cfg, nil, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("batch up infinite 2 idle node[1-2]"))
	})

	It("splits one row per node state, sorted, from a real qstat -f states column", func() {
		cfg := config.Default()
		cfg.Partitions = map[string]config.Partition{"batch": {Queue: "all.q"}}
		var out bytes.Buffer
		Expect(run(fakeQstat(threeStates), cfg, nil, &out, io.Discard)).To(Equal(0))
		s := out.String()
		Expect(s).To(ContainSubstring("batch up infinite 1 allocated node2"))
		Expect(s).To(ContainSubstring("batch up infinite 1 down node3"))
		Expect(s).To(ContainSubstring("batch up infinite 1 idle node1"))
		// States are rendered in sorted order: allocated, down, idle.
		Expect(bytes.Index([]byte(s), []byte("allocated"))).To(BeNumerically("<", bytes.Index([]byte(s), []byte(" down "))))
	})

	It("orders hosts numerically within a state row (not lexically)", func() {
		const outOfOrder = `queuename qtype resv/used/tot. load_avg arch states
----
all.q@node10 BIP 0/0/8 0.1 lx-arm64
----
all.q@node2 BIP 0/0/8 0.1 lx-arm64
----
all.q@node1 BIP 0/0/8 0.1 lx-arm64
`
		cfg := config.Default()
		cfg.Partitions = map[string]config.Partition{"batch": {Queue: "all.q"}}
		var out bytes.Buffer
		Expect(run(fakeQstat(outOfOrder), cfg, nil, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("batch up infinite 3 idle node[1-2,10]"))
	})

	It("shows a placeholder (no warning) for a configured queue with no live instances", func() {
		cfg := config.Default()
		cfg.Partitions = map[string]config.Partition{"gpu": {Queue: "gpu.q"}}
		var out, errBuf bytes.Buffer
		// Query succeeds but only lists all.q, so gpu.q has no instances.
		Expect(run(fakeQstat(twoNodeIdle), cfg, nil, &out, &errBuf)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("gpu up infinite 0 n/a -"))
		Expect(errBuf.String()).NotTo(ContainSubstring("could not query"))
	})

	It("degrades to a placeholder row with a warning when the GE query fails", func() {
		cfg := config.Default()
		cfg.Partitions = map[string]config.Partition{"batch": {Queue: "all.q"}}
		failing := &fake.Runner{Responder: func(_ string, _ []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qmaster down")}
		}}
		var out, errBuf bytes.Buffer
		Expect(run(failing, cfg, nil, &out, &errBuf)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("batch up infinite 0 n/a -"))
		Expect(errBuf.String()).To(ContainSubstring("could not query node states"))
	})

	It("prints just the header when no partitions are configured", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), config.Default(), nil, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(Equal("PARTITION AVAIL TIMELIMIT NODES STATE NODELIST\n"))
	})

	It("loads config and lists its partitions end-to-end [REQ-SIN-001]", func() {
		dir := GinkgoT().TempDir()
		cfgPath := dir + "/config.yaml"
		Expect(os.WriteFile(cfgPath, []byte("partitions:\n  gpu: {queue: gpu.q, pe: gpu.pe}\n"), 0o600)).To(Succeed())
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfgPath)

		var out bytes.Buffer
		// No cluster in the unit env: Run degrades to the placeholder listing.
		Expect(Run(nil, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("gpu up infinite"))
	})
})

var _ = Describe("nodeState mapping", func() {
	DescribeTable("maps GE queue-instance state to a SLURM node state",
		func(q gedata.QueueInstance, want string) {
			Expect(nodeState(q)).To(Equal(want))
		},
		Entry("empty queue -> idle", gedata.QueueInstance{Used: 0, Total: 8}, "idle"),
		Entry("partial -> mix", gedata.QueueInstance{Used: 4, Total: 8}, "mix"),
		Entry("full -> allocated", gedata.QueueInstance{Used: 8, Total: 8}, "allocated"),
		Entry("disabled d -> drain", gedata.QueueInstance{States: "d"}, "drain"),
		Entry("calendar-disabled D -> drain", gedata.QueueInstance{States: "D"}, "drain"),
		Entry("suspended s -> drain", gedata.QueueInstance{States: "s"}, "drain"),
		Entry("subordinate-suspend S -> drain", gedata.QueueInstance{States: "S"}, "drain"),
		Entry("calendar-suspend C -> drain", gedata.QueueInstance{States: "C"}, "drain"),
		Entry("unreachable au -> down", gedata.QueueInstance{States: "au"}, "down"),
		Entry("error E -> down", gedata.QueueInstance{States: "E"}, "down"),
		Entry("config-ambiguous c -> down", gedata.QueueInstance{States: "c"}, "down"),
		Entry("orphaned o -> down", gedata.QueueInstance{States: "o"}, "down"),
		// down is checked before drain, so a co-occurrence resolves to down.
		Entry("dE (disabled+error) -> down", gedata.QueueInstance{States: "dE"}, "down"),
		// a=load alarm alone is NOT down; usage decides (empty -> idle).
		Entry("load-alarm a alone -> idle", gedata.QueueInstance{States: "a", Used: 0, Total: 8}, "idle"),
	)
})

func bytes_IndexBatchBeforeGpu(b []byte) bool {
	return bytes.Index(b, []byte("batch")) < bytes.Index(b, []byte("gpu"))
}
