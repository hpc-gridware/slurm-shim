package encoders_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/encoders"
)

var _ = Describe("encoder error paths", func() {
	DescribeTable("ExpandNodelist rejects structurally invalid input [REQ-ENC-003]",
		func(in string) {
			_, err := encoders.ExpandNodelist(in)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid hostlist"))
		},
		Entry("empty block between commas", "a,,b"),
		Entry("close before open", "a]b[1]"),
		Entry("bracket in suffix", "node[1-2]x[3]"),
		Entry("non-numeric single item", "node[1,x]"),
		Entry("empty item", "node[1,]"),
	)

	DescribeTable("ExpandCounts rejects malformed runs [REQ-FAB-006]",
		func(in string) {
			_, err := encoders.ExpandCounts(in)
			Expect(err).To(HaveOccurred())
		},
		Entry("parens without x", "8(3)"),
		Entry("non-numeric count in run", "a(x2)"),
		Entry("missing close paren", "8(x2"),
	)

	It("reports a descriptive ErrInvalidHostlist message [REQ-ENC-003]", func() {
		err := encoders.ErrInvalidHostlist{Reason: "boom"}
		Expect(err.Error()).To(Equal("invalid hostlist: boom"))
	})
})
