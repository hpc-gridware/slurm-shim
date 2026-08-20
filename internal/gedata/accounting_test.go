package gedata_test

import (
	"context"
	"time"

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
		recs, err := gedata.JobAccounting(context.Background(), acctRunner(out),
			gedata.AcctQuery{JobID: "4711"})
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
		_, err := gedata.JobAccounting(context.Background(), r, gedata.AcctQuery{JobID: "4711"})
		Expect(err).To(HaveOccurred())
	})

	It("refuses an unfiltered query rather than dumping the accounting file", func() {
		_, err := gedata.JobAccounting(context.Background(), acctRunner(""), gedata.AcctQuery{})
		Expect(err).To(HaveOccurred())
	})

	DescribeTable("renders SLURM's code:signal exit code",
		func(failed, exit, want string) {
			recs := get(qacctSep + "\njobnumber 4711\ntaskid undefined\nfailed " +
				failed + "\nexit_status " + exit + "\n")
			Expect(recs[0].ExitCode).To(Equal(want))
		},
		Entry("clean exit", "0", "0", "0:0"),
		Entry("non-zero exit", "0", "1", "1:0"),
		Entry("self-signaled (128+SIGTERM)", "0", "143", "0:15"),
		Entry("qdel-killed", "100", "137", "0:9"),
		Entry("h_rt timeout", "37", "137", "0:9"),
		Entry("infra failure reports the GE failed code", "8", "0", "8:0"),
	)

	It("takes Elapsed from ru_wallclock, the duration the execd measured", func() {
		// A real 9.1.4 record carries both. start/end are wall-clock LABELS, so
		// subtracting them is wrong across a DST change; ru_wallclock is a real
		// measured duration and has no such failure mode.
		out := qacctSep + "\njobnumber 4711\ntaskid undefined\n" +
			"start_time 2026-08-20 19:44:47.000000\nend_time 2026-08-20 19:44:59.000000\n" +
			"ru_wallclock 7\nfailed 0\nexit_status 0\n"
		Expect(get(out)[0].Elapsed).To(Equal(7 * time.Second))
	})

	It("survives a DST transition without inventing an extra hour", func() {
		// Europe/Berlin spring-forward: the labels are two hours apart, the job
		// ran one. Subtracting the printed stamps would report 02:00:00.
		out := qacctSep + "\njobnumber 4711\ntaskid undefined\n" +
			"start_time 2026-03-29 01:30:00.000000\nend_time 2026-03-29 03:30:00.000000\n" +
			"ru_wallclock 3600\nfailed 0\nexit_status 0\n"
		Expect(get(out)[0].Elapsed).To(Equal(time.Hour))
	})

	It("falls back to end-start when ru_wallclock was not recorded", func() {
		out := qacctSep + "\njobnumber 4711\ntaskid undefined\n" +
			"start_time 2026-08-20 19:44:47.000000\nend_time 2026-08-20 19:44:59.000000\n" +
			"failed 0\nexit_status 0\n"
		Expect(get(out)[0].Elapsed).To(Equal(12 * time.Second))
	})

	It("reads maxrss as bytes and cpu time as user plus system", func() {
		out := qacctSep + "\njobnumber 4711\ntaskid undefined\nfailed 0\nexit_status 0\n" +
			"ru_utime 120.5\nru_stime 9.5\nmaxrss 2097152\n"
		Expect(get(out)[0].MaxRSS).To(Equal(float64(2097152)))
		Expect(get(out)[0].TotalCPU).To(Equal(130 * time.Second))
	})

	It("skips PE task records, which are steps rather than jobs", func() {
		// A cluster with accounting_summary FALSE writes one extra record per
		// `qrsh -inherit` task, sharing the job's number and task id. Counting
		// them would duplicate the job and let a slave host stand in for it.
		out := qacctSep + "\njobnumber 4711\ntaskid undefined\npe_taskid NONE\n" +
			"hostname node01\nfailed 0\nexit_status 0\n" +
			qacctSep + "\njobnumber 4711\ntaskid undefined\npe_taskid 1.node02\n" +
			"hostname node02\nfailed 0\nexit_status 0\n" +
			qacctSep + "\njobnumber 4711\ntaskid undefined\npe_taskid 2.node03\n" +
			"hostname node03\nfailed 0\nexit_status 0\n"
		recs := get(out)
		Expect(recs).To(HaveLen(1))
		Expect(recs[0].Host).To(Equal("node01"))
	})

	It("reports a real qacct failure instead of an empty result", func() {
		// "job id not found" and a rejected -b bound both exit 1 with empty
		// stdout; only the second is an error, and reporting it as "no jobs"
		// would answer a query that never ran.
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte(
				"Couldn't generate date from input.\nusage: qacct [options]\n")}
		}}
		_, err := gedata.JobAccounting(context.Background(), r, gedata.AcctQuery{JobID: "4711"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Couldn't generate date"))
	})

	It("treats job-id-not-found as no records, not as an error", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("error: job id 999999 not found\n")}
		}}
		recs, err := gedata.JobAccounting(context.Background(), r, gedata.AcctQuery{JobID: "999999"})
		Expect(err).NotTo(HaveOccurred())
		Expect(recs).To(BeEmpty())
	})

	It("carries the descriptive fields sacct formats", func() {
		out := qacctSep + "\nqname smp.q\nhostname node1\ngroup users\nowner gridware\n" +
			"project NONE\naccount sge\njobname train.sh\njobnumber 4711\ntaskid undefined\n" +
			"slots 4\nfailed 0\nexit_status 0\n"
		recs := get(out)
		Expect(recs).To(HaveLen(1))
		Expect(recs[0].JobName).To(Equal("train.sh"))
		Expect(recs[0].User).To(Equal("gridware"))
		Expect(recs[0].Queue).To(Equal("smp.q"))
		Expect(recs[0].Account).To(Equal("sge"))
		Expect(recs[0].Host).To(Equal("node1"))
		Expect(recs[0].Slots).To(Equal(int64(4)))
	})
})

var _ = Describe("qacct arguments", func() {
	// The flags must actually reach qacct: a silently dropped -o would turn
	// "sacct -u alice" into a dump of every user's accounting records.
	args := func(q gedata.AcctQuery) []string {
		var got []string
		r := &fake.Runner{Responder: func(_ string, a []string) fake.Response {
			got = a
			return fake.Response{}
		}}
		_, err := gedata.JobAccounting(context.Background(), r, q)
		Expect(err).NotTo(HaveOccurred())
		return got
	}

	It("maps a job id to -j", func() {
		Expect(args(gedata.AcctQuery{JobID: "4711"})).To(Equal([]string{"-j", "4711"}))
	})

	It("maps a user to a bare -j plus -o", func() {
		// The bare -j is load-bearing: "qacct -o alice" alone prints an
		// aggregate usage summary for alice, not her job records.
		Expect(args(gedata.AcctQuery{User: "alice"})).To(Equal([]string{"-j", "-o", "alice"}))
	})

	It("maps a time window to -b/-e in a stable order", func() {
		Expect(args(gedata.AcctQuery{User: "alice", Begin: "202608170000", End: "202608180000"})).
			To(Equal([]string{"-j", "-o", "alice", "-b", "202608170000", "-e", "202608180000"}))
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
