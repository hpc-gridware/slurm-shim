package gedata_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// qacctSep is the record delimiter qacct prints between job entries.
const qacctSep = "=============================================================="

// acctRunner returns a runner whose qacct replies with out, ignoring the id.
func acctRunner(out string) *fake.Runner {
	return &fake.Runner{Responder: func(name string, _ []string) fake.Response {
		return fake.Response{Stdout: []byte(out)}
	}}
}

var _ = Describe("JobAccounting", func() {
	get := func(out string) []gedata.AccountingRecord {
		recs, err := gedata.JobAccounting(context.Background(), acctRunner(out), "4711")
		Expect(err).NotTo(HaveOccurred())
		return recs
	}

	It("synthesizes COMPLETED for a clean exit", func() {
		recs := get(qacctSep + "\njobnumber 4711\ntaskid undefined\nfailed 0\nexit_status 0\n")
		Expect(recs).To(HaveLen(1))
		Expect(recs[0].State).To(Equal("COMPLETED"))
		Expect(recs[0].TaskID).To(Equal(int64(0)))
	})

	It("synthesizes FAILED for a non-zero exit with failed=0", func() {
		recs := get(qacctSep + "\njobnumber 4711\ntaskid undefined\nfailed 0\nexit_status 1\n")
		Expect(recs[0].State).To(Equal("FAILED"))
	})

	It("synthesizes CANCELLED for a killed job (failed=100)", func() {
		recs := get(qacctSep + "\njobnumber 4711\ntaskid undefined\nfailed 100\nexit_status 137\n")
		Expect(recs[0].State).To(Equal("CANCELLED"))
	})

	It("synthesizes TIMEOUT for a qmaster-enforced limit (failed=37)", func() {
		recs := get(qacctSep + "\njobnumber 4711\ntaskid undefined\nfailed 37\nexit_status 137\n")
		Expect(recs[0].State).To(Equal("TIMEOUT"))
	})

	It("synthesizes FAILED for an infra failure (prolog, failed=8)", func() {
		recs := get(qacctSep + "\njobnumber 4711\ntaskid undefined\nfailed 8\nexit_status 0\n")
		Expect(recs[0].State).To(Equal("FAILED"))
	})

	It("synthesizes NODE_FAIL for a lost job (failed=22)", func() {
		recs := get(qacctSep + "\njobnumber 4711\ntaskid undefined\nfailed 22\nexit_status 0\n")
		Expect(recs[0].State).To(Equal("NODE_FAIL"))
	})

	It("synthesizes the non-terminal REQUEUED for reschedule/migrate (failed=24/25)", func() {
		for _, code := range []string{"24", "25"} {
			recs := get(qacctSep + "\njobnumber 4711\ntaskid undefined\nfailed " + code + "\nexit_status 0\n")
			Expect(recs[0].State).To(Equal("REQUEUED"))
		}
	})

	It("returns no records for empty (job-not-found) output", func() {
		Expect(get("")).To(BeEmpty())
	})

	It("parses per-task array records with their GE task ids", func() {
		out := qacctSep + "\njobnumber 4712\ntaskid 1\nfailed 0\nexit_status 0\n" +
			qacctSep + "\njobnumber 4712\ntaskid 2\nfailed 100\nexit_status 137\n"
		recs := get(out)
		Expect(recs).To(HaveLen(2))
		Expect(recs[0].TaskID).To(Equal(int64(1)))
		Expect(recs[0].State).To(Equal("COMPLETED"))
		Expect(recs[1].TaskID).To(Equal(int64(2)))
		Expect(recs[1].State).To(Equal("CANCELLED"))
	})

	It("propagates a spawn error", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: -1, Err: context.Canceled}
		}}
		_, err := gedata.JobAccounting(context.Background(), r, "4711")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("AcctActiveState", func() {
	DescribeTable("maps GE state codes without ever returning SUSPENDED",
		func(ge, want string) { Expect(gedata.AcctActiveState(ge)).To(Equal(want)) },
		Entry("running", "r", "RUNNING"),
		Entry("transferring", "t", "RUNNING"),
		Entry("suspended maps to RUNNING", "s", "RUNNING"),
		Entry("queue-suspended maps to RUNNING", "S", "RUNNING"),
		Entry("threshold-suspended maps to RUNNING", "T", "RUNNING"),
		Entry("queued", "qw", "PENDING"),
		Entry("held", "hqw", "PENDING"),
		Entry("error stays incomplete", "Eqw", "PENDING"),
		Entry("deleting", "dr", "COMPLETING"),
		Entry("empty stays incomplete (never COMPLETED)", "", "PENDING"),
	)
})
