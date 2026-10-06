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
