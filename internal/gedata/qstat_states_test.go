package gedata_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

var _ = Describe("GE to SLURM state mapping [REQ-SQU-001]", func() {
	DescribeTable("maps GE state codes",
		func(ge, want string) {
			Expect(gedata.MapState(ge)).To(Equal(want))
		},
		Entry("waiting", "qw", "PD"),
		Entry("held waiting", "hqw", "PD"),
		Entry("restart pending", "Rq", "PD"),
		Entry("error still queued", "Eqw", "PD"),
		Entry("running", "r", "R"),
		Entry("restarted running", "Rr", "R"),
		Entry("transferring", "t", "R"),
		Entry("suspended", "s", "S"),
		Entry("suspended by admin", "S", "S"),
		Entry("suspended in transfer", "tS", "S"),
		Entry("threshold suspend", "T", "S"),
		Entry("deleting running", "dr", "CG"),
		Entry("deleting transfer", "dt", "CG"),
		Entry("absent means completed", "", "CD"),
	)

	DescribeTable("expands compact states to long form",
		func(compact, want string) {
			Expect(gedata.FullState(compact)).To(Equal(want))
		},
		Entry("pending", "PD", "PENDING"),
		Entry("running", "R", "RUNNING"),
		Entry("suspended", "S", "SUSPENDED"),
		Entry("completing", "CG", "COMPLETING"),
		Entry("completed", "CD", "COMPLETED"),
		Entry("failed", "F", "FAILED"),
		Entry("unknown passthrough", "XX", "XX"),
	)
})

var _ = Describe("qstat -xml parsing [REQ-SQU-002]", func() {
	It("parses running and pending jobs including array tasks", func() {
		xml := `<job_info>
  <queue_info>
    <job_list state="running">
      <JB_job_number>4711</JB_job_number><JB_name>train</JB_name>
      <JB_owner>alice</JB_owner><state>r</state>
      <queue_name>gpu.q@node001</queue_name><slots>8</slots>
    </job_list>
  </queue_info>
  <job_info>
    <job_list state="pending">
      <JB_job_number>4713</JB_job_number><JB_name>arr</JB_name>
      <JB_owner>bob</JB_owner><state>qw</state><slots>1</slots><tasks>2</tasks>
    </job_list>
  </job_info>
</job_info>`
		rows, err := gedata.ParseQstatXML([]byte(xml))
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(2))
		Expect(rows[0].JobID).To(Equal("4711"))
		Expect(rows[0].State).To(Equal("r"))
		Expect(rows[0].Queue).To(Equal("gpu.q@node001"))
		Expect(rows[0].Slots).To(Equal(8))
		Expect(rows[1].TaskID).To(Equal("2"))
	})

	It("returns an empty set for no jobs", func() {
		rows, err := gedata.ParseQstatXML([]byte(`<job_info></job_info>`))
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())
	})

	It("errors on malformed XML", func() {
		_, err := gedata.ParseQstatXML([]byte(`<job_info><not closed`))
		Expect(err).To(HaveOccurred())
	})
})
