package sinfo

import (
	"bytes"
	"io"
	"os"
	"strings"
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

var _ = Describe("sinfo flags [todo 078]", func() {
	// sinfo previously parsed NOTHING: every flag was dropped and the full human
	// table printed regardless, so `sinfo -h -o '%P'` returned a header row plus
	// six columns. squeue has honoured -h/-o since it was written, which made the
	// shim inconsistent with itself.
	twoPartitions := func() *config.Config {
		cfg := config.Default()
		cfg.Partitions = map[string]config.Partition{
			"batch": {Queue: "all.q", PE: "smp.pe"},
			"gpu":   {Queue: "all.q", PE: "gpu.pe"},
		}
		return cfg
	}

	It("prints no header with -h", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-h"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).NotTo(ContainSubstring("PARTITION"))
		Expect(out.String()).To(ContainSubstring("batch"))
	})

	It("prints only the requested field with -o", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-h", "-o", "%P"}, &out, io.Discard)).To(Equal(0))
		// One partition name per line and nothing else -- this is the shape a
		// script consumes.
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			Expect(strings.Fields(line)).To(HaveLen(1), "line %q should carry one field", line)
		}
		Expect(out.String()).To(ContainSubstring("batch"))
		Expect(out.String()).To(ContainSubstring("gpu"))
	})

	It("suppresses the header whenever a format is given", func() {
		// A caller that passes -o without -h is still parsing fields, so a header
		// row would be read as data.
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-o", "%P"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).NotTo(ContainSubstring("PARTITION"))
	})

	It("accepts --format= and the attached -o form", func() {
		for _, args := range [][]string{{"-h", "--format=%P"}, {"-h", "-o%P"}} {
			var out bytes.Buffer
			Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), args, &out, io.Discard)).To(Equal(0))
			Expect(out.String()).To(ContainSubstring("batch"), "args %v", args)
		}
	})

	It("expands several specifiers in one format", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(threeStates), twoPartitions(), []string{"-h", "-o", "%P/%D/%T"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(MatchRegexp(`batch/\d+/\w+`))
	})

	It("ignores a width modifier rather than rejecting it", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-h", "-o", "%.10P"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(ContainSubstring("batch"))
	})

	It("ERRORS on an unsupported specifier rather than emitting it literally", func() {
		// %G (gres) is a real sinfo specifier the shim cannot answer. Passing the
		// literal "%G" through, or dropping it, would both hand the caller a
		// wrong parse with no indication.
		var out, errb bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-h", "-o", "%G"}, &out, &errb)).To(Equal(2))
		Expect(errb.String()).To(ContainSubstring("unsupported format specifier %G"))
		Expect(errb.String()).To(ContainSubstring("%P"), "the error should list what IS supported")
	})

	It("ERRORS on an unknown flag rather than ignoring it", func() {
		var out, errb bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"--wat"}, &out, &errb)).To(Equal(2))
		Expect(errb.String()).To(ContainSubstring("unrecognized option"))
	})

	It("errors when -o is given without a value", func() {
		var out, errb bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-o"}, &out, &errb)).To(Equal(2))
		Expect(errb.String()).To(ContainSubstring("requires an argument"))
	})

	It("leaves the default output unchanged when no flags are given", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), nil, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(HavePrefix("PARTITION AVAIL TIMELIMIT NODES STATE NODELIST\n"))
	})
})

var _ = Describe("sinfo -p and format edge cases [todo 078 review]", func() {
	twoPartitions := func() *config.Config {
		cfg := config.Default()
		cfg.Partitions = map[string]config.Partition{
			"batch": {Queue: "all.q", PE: "smp.pe"},
			"gpu":   {Queue: "all.q", PE: "gpu.pe"},
		}
		return cfg
	}

	It("restricts the listing with -p", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-h", "-p", "gpu"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("gpu"))
		Expect(out.String()).NotTo(ContainSubstring("batch"))
	})

	It("accepts -p a,b and the attached -pgpu form", func() {
		for _, args := range [][]string{{"-h", "-p", "batch,gpu"}, {"-h", "-pgpu"}} {
			var out bytes.Buffer
			Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), args, &out, io.Discard)).To(Equal(0))
			Expect(out.String()).To(ContainSubstring("gpu"), "args %v", args)
		}
	})

	It("rejects an unknown partition rather than printing an empty listing", func() {
		// An empty result reads as "the partition is empty"; the truth is "there
		// is no such partition".
		var out, errb bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-p", "nope"}, &out, &errb)).To(Equal(2))
		Expect(errb.String()).To(ContainSubstring("invalid partition name"))
	})

	It("writes NOTHING to stdout when -p is invalid", func() {
		// The header used to be printed before the name was validated, so a
		// failing invocation still emitted a row a parser would consume.
		var out, errb bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-p", "nope"}, &out, &errb)).To(Equal(2))
		Expect(out.String()).To(BeEmpty())
	})

	It("suppresses the header for an explicitly EMPTY format", func() {
		// -o '' asked for a format. Falling back to the default table, header and
		// all, contradicts the request.
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-o", ""}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).NotTo(ContainSubstring("PARTITION"))
	})

	It("renders %% as a literal percent", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-h", "-o", "x%%y"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("x%y"))
	})

	It("errors on a trailing bare %", func() {
		var out, errb bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"-h", "-o", "%"}, &out, &errb)).To(Equal(2))
		Expect(errb.String()).To(ContainSubstring("bare %"))
	})

	It("names the supported flags when rejecting an unknown one", func() {
		var out, errb bytes.Buffer
		Expect(run(fakeQstat(twoNodeIdle), twoPartitions(), []string{"--sort"}, &out, &errb)).To(Equal(2))
		Expect(errb.String()).To(ContainSubstring("-p/--partition"))
	})
})
