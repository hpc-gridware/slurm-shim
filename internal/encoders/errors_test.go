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
