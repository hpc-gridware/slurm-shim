package gedata_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

var _ = Describe("PE_HOSTFILE parsing", func() {
	It("parses host, slots, queue instance, and opaque processor range [REQ-LAY-002]", func() {
		hosts, err := gedata.ParsePEHostfile([]byte(
			"node001.cluster.local 8 all.q@node001 0-7\n" +
				"node002.cluster.local 8 all.q@node002 0-7\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts).To(HaveLen(2))
		Expect(hosts[0].Name).To(Equal("node001"))
		Expect(hosts[0].FQDN).To(Equal("node001.cluster.local"))
		Expect(hosts[0].Slots).To(Equal(8))
		Expect(hosts[0].QueueInstance).To(Equal("all.q@node001"))
		Expect(hosts[0].ClusterQueue).To(Equal("all.q"))
		Expect(hosts[0].ProcessorRange).To(Equal("0-7"))
	})

	It("merges duplicate hosts by summing slots, first-seen order [REQ-LAY-003]", func() {
		hosts, err := gedata.ParsePEHostfile([]byte(
			"node001 4 all.q@node001 UNDEFINED\n" +
				"node002 8 all.q@node002 UNDEFINED\n" +
				"node001 4 gpu.q@node001 UNDEFINED\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts).To(HaveLen(2))
		Expect(hosts[0].Name).To(Equal("node001"))
		Expect(hosts[0].Slots).To(Equal(8))
		// First-seen queue metadata is kept.
		Expect(hosts[0].QueueInstance).To(Equal("all.q@node001"))
		Expect(hosts[1].Name).To(Equal("node002"))
	})

	It("tolerates a missing processor-range column and opaque tokens [REQ-LAY-002]", func() {
		hosts, err := gedata.ParsePEHostfile([]byte(
			"node001 8 all.q@node001\n" +
				"node002 8 all.q@node002 <nullptr>\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts[0].ProcessorRange).To(Equal(""))
		Expect(hosts[1].ProcessorRange).To(Equal("<nullptr>"))
	})

	It("skips blank and whitespace-only lines", func() {
		hosts, err := gedata.ParsePEHostfile([]byte("\nnode001 8\n   \n\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts).To(HaveLen(1))
		Expect(hosts[0].Slots).To(Equal(8))
	})

	DescribeTable("rejects malformed input",
		func(in string) {
			_, err := gedata.ParsePEHostfile([]byte(in))
			Expect(err).To(HaveOccurred())
		},
		Entry("empty file", ""),
		Entry("whitespace only", "   \n\n  \n"),
		Entry("missing slot count", "node001\n"),
		Entry("non-numeric slots", "node001 eight\n"),
	)
})
