package fabricator_test

import (
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("1000-node fabrication scale [M8]", func() {
	// bigHostfile builds a PE_HOSTFILE with n nodes, 8 slots each.
	bigHostfile := func(n int) string {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			host := "node" + strconv.Itoa(i)
			b.WriteString(host + " 8 all.q@" + host + " 0-7\n")
		}
		return b.String()
	}

	It("fabricates a 1000-node allocation in well under a second", func() {
		env := map[string]string{"JOB_ID": "4711", "JOB_NAME": "big", "PE": "mpi.pe"}
		start := time.Now()
		r, err := fab(env, bigHostfile(1000), testConfig())
		elapsed := time.Since(start)

		Expect(err).NotTo(HaveOccurred())
		Expect(r.Layout.Nodes).To(HaveLen(1000))
		// task_policy slot -> one task per slot -> 8000 tasks.
		m := exportMap(r)
		Expect(m["SLURM_NNODES"]).To(Equal("1000"))
		Expect(m["SLURM_NTASKS"]).To(Equal("8000"))
		// The compressed nodelist round-trips to 1000 hosts (encoder N1).
		Expect(m["SLURM_JOB_NODELIST"]).To(HavePrefix("node["))

		Expect(elapsed).To(BeNumerically("<", time.Second),
			"1000-node fabrication took %s (budget 1s)", elapsed)
	})
})

var _ = Describe("fabrication throughput benchmark helper", func() {
	It("stays fast across repeated fabrications", func() {
		env := map[string]string{"JOB_ID": "1", "PE": "mpi.pe"}
		host := ""
		for i := 1; i <= 256; i++ {
			host += "n" + strconv.Itoa(i) + " 4 all.q@n" + strconv.Itoa(i) + " 0-3\n"
		}
		start := time.Now()
		for i := 0; i < 20; i++ {
			_, err := fab(env, host, testConfig())
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(time.Since(start)).To(BeNumerically("<", 2*time.Second))
	})
})
