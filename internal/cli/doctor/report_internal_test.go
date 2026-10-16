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

package doctor

import (
	"bytes"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("report output timing", func() {
	It("prints the text report as it goes, so an interrupted run still shows what it found", func() {
		var out bytes.Buffer
		r := newReport(&out, false, false)
		r.section("versions")
		r.fail("OCS clients not reachable")
		Expect(out.String()).To(ContainSubstring("== versions\nFAIL  OCS clients not reachable\n"),
			"visible before finish")
		Expect(r.finish()).To(Equal(1))
		Expect(out.String()).To(HaveSuffix("\n1 FAIL, 0 WARN\n"))
	})

	It("writes nothing in JSON mode until the one document at the end", func() {
		var out bytes.Buffer
		r := newReport(&out, true, false)
		r.section("versions")
		r.warn("old build")
		Expect(out.Len()).To(BeZero())
		Expect(r.finish()).To(Equal(0))
		var doc Document
		Expect(json.Unmarshal(out.Bytes(), &doc)).To(Succeed())
		Expect(doc.Summary).To(Equal(Summary{Warn: 1}))
	})
})
