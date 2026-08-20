package sacct

import (
	"bytes"
	"io"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// acctFull is a qacct record with every field sacct can format, so the tests
// assert on real parsed values rather than on the empty defaults. The shapes are
// copied from a real OCS 9.1.4 record: timestamps are printed wall-clock strings
// (not epochs) and maxrss is in bytes.
func acctFull(number string) string {
	return qacctSep + "\n" +
		"qname smp.q\nhostname node07\ngroup users\nowner alice\n" +
		"project NONE\ndepartment defaultdepartment\njobname train.sh\n" +
		"jobnumber " + number + "\naccount sge\ntaskid undefined\nslots 4\n" +
		"qsub_time 2026-08-17 09:28:20.000000\n" +
		"start_time 2026-08-17 09:30:00.000000\n" +
		"end_time 2026-08-17 10:31:40.000000\n" +
		"failed 0\nexit_status 0\nru_wallclock 3700\nru_utime 120.5\nru_stime 9.5\n" +
		"maxrss 2097152\n"
}

// oneJob is a runner whose only finished record is acctFull(number).
func oneJob(number string) *fake.Runner {
	return runner(emptyQstat, map[string]string{number: acctFull(number)})
}

// column returns the value of the named field for the first data row of a
// --parsable2 run, i.e. the cell under that header.
func column(lines []string, field string) string {
	if len(lines) < 2 {
		return ""
	}
	head := strings.Split(lines[0], "|")
	vals := strings.Split(lines[1], "|")
	for i, h := range head {
		if strings.EqualFold(h, field) && i < len(vals) {
			return vals[i]
		}
	}
	return ""
}

var _ = Describe("sacct --format", func() {
	It("uses SLURM's default column set when -o is absent", func() {
		_, lines := runSacct(oneJob("4711"), "--parsable2", "-j", "4711")
		Expect(lines[0]).To(Equal("JobID|JobName|Partition|Account|AllocCPUS|State|ExitCode"))
		Expect(lines[1]).To(Equal("4711|train.sh|smp.q|sge|4|COMPLETED|0:0"))
	})

	It("prints a readable table with a dashed rule when not parsable", func() {
		_, lines := runSacct(oneJob("4711"), "-j", "4711")
		Expect(lines).To(HaveLen(3))
		Expect(lines[0]).To(ContainSubstring("JobID"))
		Expect(lines[1]).To(MatchRegexp(`^-+( -+)+$`))
		Expect(lines[2]).To(ContainSubstring("train.sh"))
		// Every line of the table is the same width: the columns line up.
		Expect(len(lines[1])).To(Equal(len(lines[0])))
		Expect(len(lines[2])).To(Equal(len(lines[0])))
	})

	It("keeps columns aligned with the header suppressed", func() {
		_, headed := runSacct(oneJob("4711"), "-j", "4711")
		_, bare := runSacct(oneJob("4711"), "-n", "-j", "4711")
		Expect(bare).To(HaveLen(1))
		Expect(bare[0]).To(Equal(headed[2]))
	})

	DescribeTable("renders each requested field",
		func(field, want string) {
			_, lines := runSacct(oneJob("4711"), "-o", field, "--parsable2", "-j", "4711")
			Expect(lines).To(HaveLen(2))
			Expect(lines[1]).To(Equal(want))
		},
		Entry("JobID", "JobID", "4711"),
		Entry("JobName", "JobName", "train.sh"),
		Entry("Partition", "Partition", "smp.q"),
		Entry("Account", "Account", "sge"),
		Entry("AllocCPUS", "AllocCPUS", "4"),
		Entry("State", "State", "COMPLETED"),
		Entry("ExitCode", "ExitCode", "0:0"),
		Entry("NodeList", "NodeList", "node07"),
		Entry("User", "User", "alice"),
		Entry("Elapsed uses GE's wallclock as [DD-]HH:MM:SS", "Elapsed", "01:01:40"),
		Entry("TotalCPU sums user and system time", "TotalCPU", "00:02:10"),
		Entry("MaxRSS is reported in K", "MaxRSS", "2048K"),
	)

	It("reproduces qacct's wall-clock times whatever zone the shim host is in", func() {
		// qacct prints cluster-local wall clock; shifting it by the shim host's
		// UTC offset would report a job as having run hours off from its log.
		_, lines := runSacct(oneJob("4711"), "-o", "Submit,Start,End", "--parsable2", "-j", "4711")
		Expect(lines[1]).To(Equal(
			"2026-08-17T09:28:20|2026-08-17T09:30:00|2026-08-17T10:31:40"))
	})

	It("reports an unrecorded timestamp as Unknown, not as the epoch", func() {
		acct := map[string]string{"4711": acctRecord("4711", "undefined", "0", "0")}
		_, lines := runSacct(runner(emptyQstat, acct), "-o", "End", "--parsable2", "-j", "4711")
		Expect(lines[1]).To(Equal("Unknown"))
	})

	It("titles columns in SLURM's own case whatever case was asked for", func() {
		_, lines := runSacct(oneJob("4711"), "-o", "jobid,STATE,ExitCode", "--parsable2", "-j", "4711")
		Expect(lines[0]).To(Equal("JobID|State|ExitCode"))
	})

	DescribeTable("resolves field aliases",
		func(alias, canonical string) {
			_, lines := runSacct(oneJob("4711"), "-o", alias, "--parsable2", "-j", "4711")
			Expect(lines[0]).To(Equal(canonical))
			_, want := runSacct(oneJob("4711"), "-o", canonical, "--parsable2", "-j", "4711")
			Expect(lines[1]).To(Equal(want[1]))
		},
		Entry("JobIDRaw", "JobIDRaw", "JobID"),
		Entry("NCPUS", "NCPUS", "AllocCPUS"),
		Entry("ReqCPUS", "ReqCPUS", "AllocCPUS"),
		Entry("Nodes", "Nodes", "NodeList"),
		Entry("CPUTime", "CPUTime", "TotalCPU"),
		Entry("UserName", "UserName", "User"),
	)

	It("accepts a display-width suffix and ignores it", func() {
		_, lines := runSacct(oneJob("4711"), "-o", "JobID%20,State%-15", "--parsable2", "-j", "4711")
		Expect(lines[0]).To(Equal("JobID|State"))
		Expect(lines[1]).To(Equal("4711|COMPLETED"))
	})

	It("prints an unknown field as an empty column rather than failing", func() {
		// sacct versions differ in the fields they know; a cosmetic column a
		// caller asks for must never take down the whole report.
		code, lines := runSacct(oneJob("4711"), "-o", "JobID,Reservation,State", "--parsable2", "-j", "4711")
		Expect(code).To(Equal(0))
		Expect(lines[0]).To(Equal("JobID|Reservation|State"))
		Expect(lines[1]).To(Equal("4711||COMPLETED"))
	})

	It("falls back to the default set when --format is empty", func() {
		_, lines := runSacct(oneJob("4711"), "-o", "", "--parsable2", "-j", "4711")
		Expect(lines[0]).To(Equal("JobID|JobName|Partition|Account|AllocCPUS|State|ExitCode"))
	})

	It("tolerates a trailing comma in the field list", func() {
		_, lines := runSacct(oneJob("4711"), "-o", "JobID,State,", "--parsable2", "-j", "4711")
		Expect(lines[0]).To(Equal("JobID|State"))
	})

	It("falls back to the default set for a spec of only separators", func() {
		_, lines := runSacct(oneJob("4711"), "-o", ",,", "--parsable2", "-j", "4711")
		Expect(lines[0]).To(Equal("JobID|JobName|Partition|Account|AllocCPUS|State|ExitCode"))
	})
})

var _ = Describe("sacct parsable flags", func() {
	It("keeps a trailing delimiter for -P", func() {
		_, lines := runSacct(oneJob("4711"), "-o", "JobID,State", "-P", "-j", "4711")
		Expect(lines).To(Equal([]string{"JobID|State|", "4711|COMPLETED|"}))
	})

	It("drops the trailing delimiter for --parsable2", func() {
		_, lines := runSacct(oneJob("4711"), "-o", "JobID,State", "--parsable2", "-j", "4711")
		Expect(lines).To(Equal([]string{"JobID|State", "4711|COMPLETED"}))
	})

	It("accepts --parsable as the long form of -P", func() {
		_, lines := runSacct(oneJob("4711"), "-o", "JobID,State", "--parsable", "-j", "4711")
		Expect(lines).To(Equal([]string{"JobID|State|", "4711|COMPLETED|"}))
	})

	It("accepts -X as a no-op (we never emit step rows)", func() {
		_, with := runSacct(oneJob("4711"), "-X", "-o", "JobID,State", "--parsable2", "-j", "4711")
		_, without := runSacct(oneJob("4711"), "-o", "JobID,State", "--parsable2", "-j", "4711")
		Expect(with).To(Equal(without))
	})

	It("never emits a step row for a job", func() {
		// A caller that counts rows (submitit does) must not see .batch/.extern.
		_, lines := runSacct(oneJob("4711"), "-o", "JobID", "--parsable2", "-j", "4711")
		Expect(lines).To(Equal([]string{"JobID", "4711"}))
	})
})

var _ = Describe("sacct selection", func() {
	// selector captures the qacct arguments a run produced, which is how the -u
	// and -S/-E mappings are pinned: they must reach qacct, not be swallowed.
	selector := func(args ...string) []string {
		var got []string
		r := &fake.Runner{Responder: func(name string, a []string) fake.Response {
			switch name {
			case "qstat":
				return fake.Response{Stdout: []byte(emptyQstat)}
			case "qacct":
				got = a
			}
			return fake.Response{}
		}}
		runSacct(r, args...)
		return got
	}

	It("maps -u to qacct -o", func() {
		Expect(selector("-u", "alice")).To(Equal([]string{"-j", "-o", "alice"}))
	})

	It("maps --user= to qacct -o", func() {
		Expect(selector("--user=alice")).To(Equal([]string{"-j", "-o", "alice"}))
	})

	It("maps -S/-E to qacct -b/-e", func() {
		Expect(selector("-u", "alice", "-S", "2026-08-17T09:30:00", "-E", "2026-08-18")).
			To(Equal([]string{"-j", "-o", "alice", "-b", "202608170930", "-e", "202608180000"}))
	})

	It("prefers the explicit -j over a user filter", func() {
		// -j is the precise selector; sending -o alongside it would make qacct
		// intersect the two and silently hide another user's job.
		Expect(selector("-u", "alice", "-j", "4711")).To(Equal([]string{"-j", "4711"}))
	})

	It("rejects an unparsable time bound instead of silently dropping it", func() {
		// Dropping it would either widen the query to the whole accounting file
		// or, with no other selector, report "no jobs" for a window that was
		// never applied -- and the caller could not tell either from a real answer.
		var out, errBuf bytes.Buffer
		code := run(oneJob("4711"), []string{"-u", "alice", "-S", "not-a-time"}, &out, &errBuf)
		Expect(code).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("not-a-time"))
		Expect(out.String()).To(BeEmpty())
	})

	// bound returns the CCYYMMDDhhmm that -S produced for spec.
	bound := func(spec string) string {
		got := selector("-u", "alice", "-S", spec)
		Expect(got).To(HaveLen(5), "expected -j -o <user> -b <stamp>, got %v", got)
		Expect(got[3]).To(Equal("-b"))
		return got[4]
	}

	DescribeTable("accepts SLURM's absolute time spellings",
		func(spec, want string) { Expect(bound(spec)).To(Equal(want)) },
		Entry("ISO date", "2026-08-17", "202608170000"),
		Entry("ISO date and time", "2026-08-17T09:30", "202608170930"),
		Entry("ISO date and time with seconds", "2026-08-17T09:30:45", "202608170930"),
		Entry("MM/DD-HH:MM:SS fills in the current year",
			"08/17-09:30:00", time.Now().Format("2006")+"08170930"),
		Entry("today", "today", time.Now().Format("20060102")+"0000"),
		Entry("midnight", "midnight", time.Now().Format("20060102")+"0000"),
		Entry("yesterday", "yesterday", time.Now().AddDate(0, 0, -1).Format("20060102")+"0000"),
	)

	DescribeTable("accepts SLURM's relative time spellings",
		func(spec string, offset time.Duration) {
			// Compared with a tolerance: the stamp has minute resolution, so an
			// exact match would flake whenever the minute rolls over mid-test.
			got, err := time.ParseInLocation("200601021504", bound(spec), time.Local)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeTemporally("~", time.Now().Add(offset), 90*time.Second))
		},
		Entry("now", "now", time.Duration(0)),
		Entry("now-2hours", "now-2hours", -2*time.Hour),
		Entry("now-30minutes", "now-30minutes", -30*time.Minute),
		Entry("now-1day", "now-1day", -24*time.Hour),
		Entry("now+15min", "now+15min", 15*time.Minute),
		Entry("a bare number means minutes", "now-45", -45*time.Minute),
	)

	It("splits a comma-separated -u into one qacct call per user", func() {
		var got [][]string
		r := &fake.Runner{Responder: func(name string, a []string) fake.Response {
			switch name {
			case "qstat":
				return fake.Response{Stdout: []byte(emptyQstat)}
			case "qacct":
				got = append(got, a)
			}
			return fake.Response{}
		}}
		runSacct(r, "-u", "alice,bob")
		Expect(got).To(Equal([][]string{{"-j", "-o", "alice"}, {"-j", "-o", "bob"}}))
	})

	It("keeps live jobs of every listed user in a comma -u report", func() {
		q := qstatXML(jobList("4712", "r", "smp.q@node02", ""),
			strings.Replace(jobList("4713", "r", "smp.q@node03", ""),
				"<JB_owner>alice</JB_owner>", "<JB_owner>bob</JB_owner>", 1))
		_, lines := runSacct(runner(q, nil), "-o", "JobID,User", "--parsable2", "-u", "alice,bob")
		Expect(lines).To(ContainElement("4712|alice"))
		Expect(lines).To(ContainElement("4713|bob"))
	})

	It("excludes a running job from a window that closed before it started", func() {
		// qstat answers for right now, so the -S/-E bounds have to be applied to
		// the live rows too, not only to qacct.
		start := time.Now().Format("2006-01-02T15:04:05.000000")
		q := qstatXML(strings.Replace(jobList("4711", "r", "smp.q@node01", ""),
			"<slots>1</slots>", "<JAT_start_time>"+start+"</JAT_start_time><slots>1</slots>", 1))
		_, lines := runSacct(runner(q, nil), "-o", "JobID", "--parsable2",
			"-u", "alice", "-S", "2000-01-01", "-E", "2000-01-02")
		Expect(lines).To(Equal([]string{"JobID"}))
	})

	It("keeps a running job inside an open-ended window", func() {
		start := time.Now().Format("2006-01-02T15:04:05.000000")
		q := qstatXML(strings.Replace(jobList("4711", "r", "smp.q@node01", ""),
			"<slots>1</slots>", "<JAT_start_time>"+start+"</JAT_start_time><slots>1</slots>", 1))
		_, lines := runSacct(runner(q, nil), "-o", "JobID", "--parsable2",
			"-u", "alice", "-S", "yesterday")
		Expect(lines).To(ContainElement("4711"))
	})

	It("reports jobs selected by user, live ones included", func() {
		q := qstatXML(jobList("4712", "r", "smp.q@node02", ""))
		r := &fake.Runner{Responder: func(name string, _ []string) fake.Response {
			switch name {
			case "qstat":
				return fake.Response{Stdout: []byte(q)}
			case "qacct":
				return fake.Response{Stdout: []byte(acctFull("4711"))}
			}
			return fake.Response{}
		}}
		_, lines := runSacct(r, "-o", "JobID,User,State", "--parsable2", "-u", "alice")
		Expect(lines).To(ContainElement("4712|alice|RUNNING"))
		Expect(lines).To(ContainElement("4711|alice|COMPLETED"))
	})

	It("excludes another user's live jobs from a -u report", func() {
		q := qstatXML(jobList("4712", "r", "smp.q@node02", ""))
		r := &fake.Runner{Responder: func(name string, _ []string) fake.Response {
			if name == "qstat" {
				return fake.Response{Stdout: []byte(q)}
			}
			return fake.Response{}
		}}
		_, lines := runSacct(r, "-o", "JobID", "--parsable2", "-u", "bob")
		Expect(lines).To(Equal([]string{"JobID"}))
	})

	It("prints only the header when nothing was selected at all", func() {
		// Without a selector SLURM would report the day's jobs; refusing to walk
		// the whole accounting file is safer than guessing a window.
		_, lines := runSacct(oneJob("4711"), "-o", "JobID,State", "--parsable2")
		Expect(lines).To(Equal([]string{"JobID|State"}))
	})
})

var _ = Describe("sacct live rows", func() {
	It("formats a running job from qstat alone", func() {
		q := qstatXML(jobList("4711", "r", "smp.q@node01", ""))
		_, lines := runSacct(runner(q, nil),
			"-o", "JobID,JobName,Partition,AllocCPUS,State,NodeList,User", "--parsable2", "-j", "4711")
		Expect(lines[1]).To(Equal("4711|job|smp.q|1|RUNNING|node01|alice"))
	})

	It("leaves a running job's accounting-only fields empty, not wrong", func() {
		// GE writes usage at the end of the job, so a running job has no exit
		// code or end time; reporting 0:0 would look like a clean finish.
		q := qstatXML(jobList("4711", "r", "smp.q@node01", ""))
		_, lines := runSacct(runner(q, nil), "-o", "ExitCode,End,MaxRSS", "--parsable2", "-j", "4711")
		Expect(column(lines, "ExitCode")).To(BeEmpty())
		Expect(column(lines, "End")).To(Equal("Unknown"))
		Expect(column(lines, "MaxRSS")).To(BeEmpty())
	})

	It("counts a running job's Elapsed from its qstat start time", func() {
		start := time.Now().Add(-90 * time.Second).Format("2006-01-02T15:04:05.000000")
		q := qstatXML(strings.Replace(jobList("4711", "r", "smp.q@node01", ""),
			"<slots>1</slots>", "<JAT_start_time>"+start+"</JAT_start_time><slots>1</slots>", 1))
		_, lines := runSacct(runner(q, nil), "-o", "Start,Elapsed", "--parsable2", "-j", "4711")
		Expect(column(lines, "Start")).To(Equal(start[:19]))
		Expect(column(lines, "Elapsed")).To(Equal("00:01:30"))
	})

	It("reports a pending job's Submit time and no start", func() {
		sub := time.Now().Add(-30 * time.Second).Format("2006-01-02T15:04:05.000000")
		q := `<?xml version='1.0'?><job_info><queue_info></queue_info><job_info>` +
			strings.Replace(jobList("4711", "qw", "", ""),
				"<slots>1</slots>", "<JAT_submission_time>"+sub+"</JAT_submission_time><slots>1</slots>", 1) +
			`</job_info></job_info>`
		_, lines := runSacct(runner(q, nil), "-o", "State,Submit,Start,Elapsed", "--parsable2", "-j", "4711")
		Expect(column(lines, "State")).To(Equal("PENDING"))
		Expect(column(lines, "Submit")).To(Equal(sub[:19]))
		Expect(column(lines, "Start")).To(Equal("Unknown"))
		Expect(column(lines, "Elapsed")).To(Equal("00:00:00"))
	})
})

var _ = Describe("sacct hardening", func() {
	It("fails loudly when qacct cannot run on the -j path", func() {
		// An empty result means "unknown, keep polling", so a swallowed error
		// would turn every finished job into a permanent non-terminal answer
		// with exit 0 -- the one outcome a polling consumer cannot recover from.
		r := &fake.Runner{Responder: func(name string, _ []string) fake.Response {
			if name == "qstat" {
				return fake.Response{Stdout: []byte(emptyQstat)}
			}
			return fake.Response{Exit: -1, Err: io.ErrUnexpectedEOF}
		}}
		var out, errBuf bytes.Buffer
		code := run(r, []string{"-o", "JobID,State", "--parsable2", "-j", "4711"}, &out, &errBuf)
		Expect(code).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("sacct: error: running qacct"))
	})

	It("ignores a record belonging to a different job number", func() {
		// qacct given a non-numeric -j treats it as a job NAME and answers with
		// every match, so an unrelated record must not overwrite the requested
		// job just because both carry task id 0.
		acct := map[string]string{"500": acctRecord("500", "undefined", "0", "0") +
			acctRecord("501", "undefined", "100", "137")}
		_, lines := runSacct(runner(emptyQstat, acct), "-o", "JobID,State", "--parsable2", "-j", "500")
		Expect(lines).To(Equal([]string{"JobID|State", "500|COMPLETED"}))
	})

	It("never renders a negative array element", func() {
		// A reused GE job number can put a non-array record (task 0) and array
		// records (task >= 1) in one answer; task 0 is not element -1.
		acct := map[string]string{"4712": acctRecord("4712", "undefined", "0", "0") +
			acctRecord("4712", "1", "0", "0")}
		_, lines := runSacct(runner(emptyQstat, acct), "-o", "JobID", "--parsable2", "-j", "4712")
		for _, l := range lines {
			Expect(l).NotTo(ContainSubstring("_-"))
		}
		Expect(lines).To(ContainElement("4712"))
		Expect(lines).To(ContainElement("4712_0"))
	})

	It("collapses PE task records into the one job that owns them", func() {
		// accounting_summary FALSE (the spec's REQ-APX-004 recommendation) adds
		// one record per qrsh -inherit task; those are steps, not jobs.
		rec := func(pe, host string) string {
			return qacctSep + "\njobnumber 500\ntaskid undefined\npe_taskid " + pe +
				"\nhostname " + host + "\nslots 3\nfailed 0\nexit_status 0\n"
		}
		acct := map[string]string{"500": rec("NONE", "node01") +
			rec("1.node02", "node02") + rec("2.node03", "node03")}
		_, lines := runSacct(runner(emptyQstat, acct), "-o", "JobID,NodeList", "--parsable2", "-j", "500")
		Expect(lines).To(Equal([]string{"JobID|NodeList", "500|node01"}))
	})

	DescribeTable("reports nothing for an id naming no job this shim can report",
		func(id string) {
			// Falling back to "every element of the array" would answer a
			// different question than the one asked.
			acct := map[string]string{"4712": acctRecord("4712", "1", "0", "0") +
				acctRecord("4712", "2", "0", "0")}
			_, lines := runSacct(runner(emptyQstat, acct), "-o", "JobID", "--parsable2", "-j", id)
			Expect(lines).To(Equal([]string{"JobID"}))
		},
		Entry("a batch step", "4712_0.batch"),
		Entry("a step on a plain job", "4711.batch"),
		Entry("an array range", "4712_[0-1]"),
		Entry("junk", "4712_abc"),
	)

	DescribeTable("filters by -s/--state",
		func(flag, value string, want []string) {
			// A silently ignored selection flag is dangerous: the standard
			// idiom `sacct -s R -o JobID | xargs scancel` would cancel finished
			// jobs too.
			q := qstatXML(jobList("4712", "r", "smp.q@node02", ""))
			acct := map[string]string{"4711": acctRecord("4711", "undefined", "0", "0")}
			r := runner(q, acct)
			args := []string{"-n", "-o", "JobID", "--parsable2", "-j", "4711", "-j", "4712"}
			if flag != "" {
				args = append(args, flag, value)
			}
			_, lines := runSacct(r, args...)
			if len(want) == 0 {
				Expect(strings.Join(lines, "")).To(BeEmpty())
				return
			}
			Expect(lines).To(Equal(want))
		},
		Entry("no filter keeps both", "", "", []string{"4711", "4712"}),
		Entry("compact R keeps the running one", "-s", "R", []string{"4712"}),
		Entry("long RUNNING keeps the running one", "-s", "RUNNING", []string{"4712"}),
		Entry("CD keeps the finished one", "-s", "CD", []string{"4711"}),
		Entry("--state= form", "--state=COMPLETED", "", []string{"4711"}),
		Entry("a comma list keeps both", "-s", "R,CD", []string{"4711", "4712"}),
		Entry("an unknown state matches nothing", "-s", "NOSUCHSTATE", nil),
	)
})
