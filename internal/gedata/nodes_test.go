package gedata_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// clusterResponder replays the OCS 9.1.6 node fixtures: qhost -F, qconf -sc,
// qconf -se <gpu host> (both workers carry gpu=2(0 1)), qconf -sq all.q.
func clusterResponder(name string, args []string) fake.Response {
	switch name + " " + strings.Join(args, " ") {
	case "qhost -F":
		return fake.Response{Stdout: readFixture("qhost_F_9.1.6.txt")}
	case "qconf -sc":
		return fake.Response{Stdout: readFixture("qconf_sc_9.1.6.txt")}
	case "qconf -se ocs-worker1", "qconf -se ocs-worker2":
		return fake.Response{Stdout: readFixture("qconf_se_gpu_9.1.6.txt")}
	case "qconf -sq all.q":
		return fake.Response{Stdout: readFixture("qconf_sq_9.1.6.txt")}
	}
	return fake.Response{Exit: 1, Stderr: []byte("unexpected " + name + " " + strings.Join(args, " "))}
}

var _ = Describe("execution host records", func() {
	It("reads CPUs, memory, BOOL features and the configured GPU count", func() {
		r := &fake.Runner{Responder: clusterResponder}
		nodes, err := gedata.Nodes(context.Background(), r, "gpu")
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes).To(HaveLen(3), "the global pseudo host is not a node")

		byName := map[string]gedata.Node{}
		for _, n := range nodes {
			byName[n.Name] = n
		}
		w1 := byName["ocs-worker1"]
		Expect(w1.CPUs).To(Equal(14))
		Expect(w1.MemoryMB).To(BeNumerically("==", 12015)) // hl:mem_total=11.734G
		Expect(w1.Reachable()).To(BeTrue())
		// e2e_avx is a fixed BOOL; e2e_excl is a consumable (EXCL) BOOL, a
		// resource rather than a label.
		Expect(w1.Features).To(Equal([]string{"e2e_avx"}))
		// qhost -F shows the free amount; the total comes from complex_values.
		Expect(w1.GPUs).To(Equal(2))

		Expect(byName["ocs-master"].Features).To(BeEmpty())
		Expect(byName["ocs-master"].GPUs).To(Equal(0))
	})

	It("asks qconf -se only for hosts that carry the GPU complex", func() {
		r := &fake.Runner{Responder: clusterResponder}
		_, err := gedata.Nodes(context.Background(), r, "gpu")
		Expect(err).NotTo(HaveOccurred())
		var se []string
		for _, c := range r.Calls {
			if c.Name == "qconf" && c.Args[0] == "-se" {
				se = append(se, c.Args[1])
			}
		}
		Expect(se).To(ConsistOf("ocs-worker1", "ocs-worker2"))
	})

	It("fails with GE's message when qhost fails", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("error: commlib error")}
		}}
		_, err := gedata.Nodes(context.Background(), r, "gpu")
		Expect(err).To(MatchError(ContainSubstring("qhost -F: exit 1: error: commlib error")))
	})

	It("reads a queue's default h_rt", func() {
		r := &fake.Runner{Responder: clusterResponder}
		Expect(gedata.QueueTimeLimit(context.Background(), r, "all.q")).To(Equal("INFINITY"))

		r = &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte("qname gpu.q\nh_rt 48:00:00,[node1=1:00:00]\n")}
		}}
		Expect(gedata.QueueTimeLimit(context.Background(), r, "gpu.q")).To(Equal("48:00:00"))
	})

	It("maps complex names to types", func() {
		r := &fake.Runner{Responder: clusterResponder}
		types, err := gedata.ComplexTypes(context.Background(), r)
		Expect(err).NotTo(HaveOccurred())
		Expect(types).To(HaveKeyWithValue("e2e_avx", "BOOL"))
		Expect(types).To(HaveKeyWithValue("gpu", "RSMAP"))
		Expect(types).To(HaveKeyWithValue("h_rt", "TIME"))
	})
})
