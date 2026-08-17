package encoders_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/encoders"
)

var _ = Describe("N2 tasks-per-node RLE", func() {
	DescribeTable("run-length encodes per-node counts [REQ-ENC-001]",
		func(in []int, want string) {
			Expect(encoders.CompressCounts(in)).To(Equal(want))
		},
		Entry("run then single", []int{8, 8, 8, 4}, "8(x3),4"),
		Entry("single value", []int{2}, "2"),
		Entry("pair", []int{4, 4}, "4(x2)"),
		Entry("no runs", []int{1, 2, 1}, "1,2,1"),
	)

	DescribeTable("round-trips ExpandCounts(CompressCounts(x)) == x [REQ-FAB-006]",
		func(in []int) {
			s := encoders.CompressCounts(in)
			got, err := encoders.ExpandCounts(s)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(in))
		},
		Entry("run then single", []int{8, 8, 8, 4}),
		Entry("single", []int{2}),
		Entry("alternating", []int{1, 2, 1}),
		Entry("long uniform", []int{4, 4, 4, 4, 4}),
	)

	DescribeTable("rejects malformed input [REQ-FAB-006]",
		func(in string) {
			_, err := encoders.ExpandCounts(in)
			Expect(err).To(HaveOccurred())
		},
		Entry("empty", ""),
		Entry("bad count", "x"),
		Entry("bad repeat", "8(xz)"),
		Entry("zero repeat", "8(x0)"),
	)
})
