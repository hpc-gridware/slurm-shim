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
