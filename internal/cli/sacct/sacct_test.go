package sacct

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func TestSacct(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Sacct Suite")
}

const qacctSep = "=============================================================="

const emptyQstat = `<?xml version='1.0'?><job_info></job_info>`

// qstatXML wraps running job_list entries in the queue_info section.
func qstatXML(running ...string) string {
	return `<?xml version='1.0'?><job_info><queue_info>` +
		strings.Join(running, "") +
		`</queue_info><job_info></job_info></job_info>`
}

func jobList(number, state, queue, tasks string) string {
	t := ""
	if tasks != "" {
		t = "<tasks>" + tasks + "</tasks>"
	}
	return "<job_list state='running'>" +
		"<JB_job_number>" + number + "</JB_job_number>" +
		"<JB_name>job</JB_name><JB_owner>alice</JB_owner>" +
		"<state>" + state + "</state>" +
		"<queue_name>" + queue + "</queue_name><slots>1</slots>" + t +
		"</job_list>"
}

func acctRecord(number, taskid, failed, exit string) string {
	return qacctSep + "\njobnumber " + number + "\ntaskid " + taskid +
		"\nfailed " + failed + "\nexit_status " + exit + "\n"
}

// runner replies to qstat with qstat and to qacct with qacctByID[<id>].
func runner(qstat string, qacctByID map[string]string) *fake.Runner {
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		switch name {
		case "qstat":
			return fake.Response{Stdout: []byte(qstat)}
		case "qacct":
			id := ""
			for i, a := range args {
				if a == "-j" && i+1 < len(args) {
					id = args[i+1]
				}
			}
			return fake.Response{Stdout: []byte(qacctByID[id])}
		}
		return fake.Response{}
	}}
}

// runSacct runs the command and returns its stdout lines (header included).
func runSacct(r *fake.Runner, args ...string) (int, []string) {
	var out bytes.Buffer
	code := run(r, args, &out, io.Discard)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	return code, lines
}

var _ = Describe("sacct", func() {
	base := []string{"-o", "JobID,State,NodeList", "--parsable2", "-j"}

	It("always prints the exact header submitit keys on", func() {
		code, lines := runSacct(runner(emptyQstat, nil), append(base, "9999")...)
		Expect(code).To(Equal(0))
		Expect(lines[0]).To(Equal("JobID|State|NodeList"))
	})

	It("omits an unknown id so the consumer keeps polling (header only)", func() {
		_, lines := runSacct(runner(emptyQstat, nil), append(base, "9999")...)
		Expect(lines).To(Equal([]string{"JobID|State|NodeList"}))
	})

	It("reports a live job from qstat as RUNNING with its node", func() {
		q := qstatXML(jobList("4711", "r", "all.q@node01", ""))
		_, lines := runSacct(runner(q, nil), append(base, "4711")...)
		Expect(lines).To(ContainElement("4711|RUNNING|node01"))
	})

	It("maps a GE-suspended job to RUNNING (never SUSPENDED)", func() {
		q := qstatXML(jobList("4711", "s", "all.q@node01", ""))
		_, lines := runSacct(runner(q, nil), append(base, "4711")...)
		Expect(lines).To(ContainElement("4711|RUNNING|node01"))
	})

	It("reports a finished single job from qacct as COMPLETED", func() {
		acct := map[string]string{"4711": acctRecord("4711", "undefined", "0", "0")}
		_, lines := runSacct(runner(emptyQstat, acct), append(base, "4711")...)
		Expect(lines).To(ContainElement("4711|COMPLETED|"))
	})

	It("reports a failure-without-pickle as a terminal state (no hang)", func() {
		acct := map[string]string{"4711": acctRecord("4711", "undefined", "100", "137")}
		_, lines := runSacct(runner(emptyQstat, acct), append(base, "4711")...)
		Expect(lines).To(ContainElement("4711|CANCELLED|"))
	})

	It("reports array elements 0-based (GE task k -> _<k-1>)", func() {
		acct := map[string]string{"4712": acctRecord("4712", "1", "0", "0") +
			acctRecord("4712", "2", "0", "0") + acctRecord("4712", "3", "100", "137")}
		_, lines := runSacct(runner(emptyQstat, acct), append(base, "4712")...)
		Expect(lines).To(ContainElement("4712_0|COMPLETED|"))
		Expect(lines).To(ContainElement("4712_1|COMPLETED|"))
		Expect(lines).To(ContainElement("4712_2|CANCELLED|"))
	})

	It("merges live and finished array elements", func() {
		q := qstatXML(jobList("4712", "r", "all.q@node02", "2"))
		acct := map[string]string{"4712": acctRecord("4712", "1", "0", "0")}
		_, lines := runSacct(runner(q, acct), append(base, "4712")...)
		Expect(lines).To(ContainElement("4712_0|COMPLETED|"))     // task 1 finished
		Expect(lines).To(ContainElement("4712_1|RUNNING|node02")) // task 2 live
	})

	It("lets a live qstat row win over a stale finished record for the same task", func() {
		q := qstatXML(jobList("4711", "r", "all.q@node01", ""))
		acct := map[string]string{"4711": acctRecord("4711", "undefined", "100", "137")}
		_, lines := runSacct(runner(q, acct), append(base, "4711")...)
		Expect(lines).To(Equal([]string{"JobID|State|NodeList", "4711|RUNNING|node01"}))
	})

	It("lets the latest qacct record win for a task that ran twice (requeue -> completed)", func() {
		// qacct prints oldest first: a requeue (failed=100) then a clean re-run.
		acct := map[string]string{"4711": acctRecord("4711", "undefined", "100", "137") +
			acctRecord("4711", "undefined", "0", "0")}
		_, lines := runSacct(runner(emptyQstat, acct), append(base, "4711")...)
		Expect(lines).To(ContainElement("4711|COMPLETED|"))
		Expect(lines).NotTo(ContainElement("4711|CANCELLED|"))
	})

	It("filters to a single requested 0-based array element (-j N_2)", func() {
		acct := map[string]string{"4712": acctRecord("4712", "1", "0", "0") +
			acctRecord("4712", "2", "0", "0") + acctRecord("4712", "3", "0", "1")}
		// -j 4712_2 (SLURM 0-based) -> GE task 3 -> exit_status 1 -> FAILED.
		_, lines := runSacct(runner(emptyQstat, acct), append(base, "4712_2")...)
		Expect(lines).To(Equal([]string{"JobID|State|NodeList", "4712_2|FAILED|"}))
	})

	It("accepts repeated -j flags", func() {
		acct := map[string]string{
			"4711": acctRecord("4711", "undefined", "0", "0"),
			"4713": acctRecord("4713", "undefined", "0", "1"),
		}
		_, lines := runSacct(runner(emptyQstat, acct), "-o", "JobID,State,NodeList", "--parsable2", "-j", "4711", "-j", "4713")
		Expect(lines).To(ContainElement("4711|COMPLETED|"))
		Expect(lines).To(ContainElement("4713|FAILED|"))
	})

	It("accepts a comma-separated -j list", func() {
		acct := map[string]string{
			"4711": acctRecord("4711", "undefined", "0", "0"),
			"4713": acctRecord("4713", "undefined", "0", "0"),
		}
		_, lines := runSacct(runner(emptyQstat, acct), append(base, "4711,4713")...)
		Expect(lines).To(ContainElement("4711|COMPLETED|"))
		Expect(lines).To(ContainElement("4713|COMPLETED|"))
	})

	It("does not let an attached -o/--format value swallow the following flag", func() {
		// -oFOO and --format=FOO carry their value, so unlike the separate form
		// they must not consume the next argument -- otherwise the -j after them
		// would be eaten and no job would be reported.
		acct := map[string]string{"4711": acctRecord("4711", "undefined", "0", "0")}
		for _, fmtFlag := range []string{"-oJobID,State,NodeList", "--format=JobID,State,NodeList"} {
			_, lines := runSacct(runner(emptyQstat, acct), fmtFlag, "--parsable2", "-j", "4711")
			Expect(lines).To(ContainElement("4711|COMPLETED|"), "with %s", fmtFlag)
		}
	})

	It("suppresses the header with --noheader", func() {
		acct := map[string]string{"4711": acctRecord("4711", "undefined", "0", "0")}
		_, lines := runSacct(runner(emptyQstat, acct), append(append([]string{"-n"}, base...), "4711")...)
		Expect(lines).To(Equal([]string{"4711|COMPLETED|"}))
	})

	It("errors cleanly when qstat cannot run", func() {
		r := &fake.Runner{Responder: func(name string, _ []string) fake.Response {
			if name == "qstat" {
				return fake.Response{Exit: -1, Err: io.ErrUnexpectedEOF}
			}
			return fake.Response{}
		}}
		var errBuf bytes.Buffer
		code := run(r, append(base, "4711"), io.Discard, &errBuf)
		Expect(code).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("sacct: error"))
	})
})

var _ = Describe("sacct options it does not translate [REQ-RUN-005]", func() {
	It("warns, steps over the value, and keeps the options it does translate", func() {
		// --clusters takes a value (SLURM arity) and --units too; -L takes none.
		opt, err := parse([]string{"--clusters", "c1", "-L", "-j", "4711", "--units=G", "-P"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.ids).To(Equal([]string{"4711"}))
		Expect(opt.parsable).To(BeTrue())
		Expect(opt.warnings).To(ConsistOf(ContainSubstring("--clusters ignored"),
			ContainSubstring("-L ignored"), ContainSubstring("--units=G ignored")))
	})
})

// fixture reads a qacct answer captured live from OCS 9.1.6 (testdata/).
func fixture(name string) string {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

// asUser makes sacct see the given caller for the rest of the spec.
func asUser(name string, root bool) {
	old := whoami
	whoami = func() (string, bool) { return name, root }
	DeferCleanup(func() { whoami = old })
}

// qacctArgsOf returns the argv of every qacct call the runner saw.
func qacctArgsOf(r *fake.Runner) [][]string {
	var out [][]string
	for _, c := range r.Calls {
		if c.Name == "qacct" {
			out = append(out, c.Args)
		}
	}
	return out
}

var _ = Describe("sacct request columns and TIMEOUT (live OCS 9.1.6 records)", func() {
	cols := "JobID,State,ExitCode,NNodes,ReqMem,Timelimit,WorkDir"

	DescribeTable("reports what the recorded request says, and nothing it does not",
		func(file, id, want string) {
			r := runner(emptyQstat, map[string]string{id: fixture(file)})
			_, lines := runSacct(r, "-o", cols, "-P", "-n", "-j", id)
			Expect(lines).To(Equal([]string{want}))
		},
		Entry("h_rt=5 killed at its limit is TIMEOUT", "qacct_1380_hrt_timeout.txt", "1380",
			"1380|TIMEOUT|0:9|1||00:00:05|"),
		Entry("qdel before h_rt stays CANCELLED", "qacct_1387_qdel.txt", "1387",
			"1387|CANCELLED|0:9|1||00:10:00|"),
		Entry("an s_rt kill is not a time-limit kill", "qacct_1381_srt_kill.txt", "1381",
			"1381|CANCELLED|0:9|1|||"),
		Entry("-wd and mem_free come from the command line", "qacct_1384_wd_mem.txt", "1384",
			"1384|COMPLETED|0:0|1|100M|00:10:00|/tmp"),
		Entry("a #$ directive is not on the command line", "qacct_1385_embedded_hrt.txt", "1385",
			"1385|COMPLETED|0:0|1|||"),
		Entry("a 4-slot -par 2 PE job spans 2 nodes", "qacct_1391_pe_tasks.txt", "1391",
			"1391|COMPLETED|0:0|2|||"),
		Entry("-par alone fixes NNodes under accounting_summary", "qacct_1382_pe_par_summary.txt", "1382",
			"1382|COMPLETED|0:0|2|||"),
	)

	It("counts NNodes from the PE task hosts when no -par fixes the geometry", func() {
		acct := strings.ReplaceAll(fixture("qacct_1391_pe_tasks.txt"), " -par 2", "")
		r := runner(emptyQstat, map[string]string{"1391": acct})
		_, lines := runSacct(r, "-o", "JobID,NNodes", "-P", "-n", "-j", "1391")
		Expect(lines).To(Equal([]string{"1391|2"}))
	})

	It("leaves NNodes empty for a multi-slot PE job with neither -par nor task records", func() {
		acct := strings.ReplaceAll(fixture("qacct_1382_pe_par_summary.txt"), " -par 2", "")
		r := runner(emptyQstat, map[string]string{"1382": acct})
		_, lines := runSacct(r, "-o", "JobID,NNodes", "-P", "-n", "-j", "1382")
		Expect(lines).To(Equal([]string{"1382|"}))
	})

	It("leaves the request columns empty for a live job (qstat carries no requests)", func() {
		q := qstatXML(jobList("4711", "r", "all.q@node01", ""))
		_, lines := runSacct(runner(q, nil), "-o", "JobID,NNodes,ReqMem,Timelimit,WorkDir", "-P", "-n", "-j", "4711")
		Expect(lines).To(Equal([]string{"4711||||"}))
	})
})

var _ = Describe("sacct -D/--duplicates (live requeued record: qmod -rj, then a clean run)", func() {
	acct := func() map[string]string { return map[string]string{"1383": fixture("qacct_1383_requeued.txt")} }

	It("shows only the latest run by default", func() {
		_, lines := runSacct(runner(emptyQstat, acct()), "-o", "JobID,State,NodeList", "-P", "-n", "-j", "1383")
		Expect(lines).To(Equal([]string{"1383|COMPLETED|ocs-worker2"}))
	})

	It("shows every run, oldest first, with -D", func() {
		for _, flag := range []string{"-D", "--duplicates"} {
			_, lines := runSacct(runner(emptyQstat, acct()), flag, "-o", "JobID,State,NodeList", "-P", "-n", "-j", "1383")
			Expect(lines).To(Equal([]string{"1383|REQUEUED|ocs-worker1", "1383|COMPLETED|ocs-worker2"}), flag)
		}
	})

	It("applies the same rule to a listing by user", func() {
		// A listing runs a bare "qacct -j -o gridware"; answer every qacct.
		requeued := fixture("qacct_1383_requeued.txt")
		r := &fake.Runner{Responder: func(name string, _ []string) fake.Response {
			if name == "qstat" {
				return fake.Response{Stdout: []byte(emptyQstat)}
			}
			return fake.Response{Stdout: []byte(requeued)}
		}}
		_, lines := runSacct(r, "-o", "JobID,State", "-P", "-n", "-u", "gridware")
		Expect(lines).To(Equal([]string{"1383|COMPLETED"}))
		_, lines = runSacct(r, "-D", "-o", "JobID,State", "-P", "-n", "-u", "gridware")
		Expect(lines).To(Equal([]string{"1383|REQUEUED", "1383|COMPLETED"}))
	})

	It("lists finished runs before the live one with -D", func() {
		q := qstatXML(jobList("1383", "r", "all.q@node01", ""))
		rec := acctRecord("1383", "undefined", "25", "0")
		_, lines := runSacct(runner(q, map[string]string{"1383": rec}), "-D", "-o", "JobID,State", "-P", "-n", "-j", "1383")
		Expect(lines).To(Equal([]string{"1383|REQUEUED", "1383|RUNNING"}))
	})
})

var _ = Describe("sacct --name", func() {
	It("selects finished jobs by exact name through qacct -j <name>", func() {
		asUser("gridware", false)
		// qacct's -j <name> also matches patterns; only the exact name survives.
		acct := map[string]string{"sx-hrt": fixture("qacct_1380_hrt_timeout.txt") + fixture("qacct_1384_wd_mem.txt")}
		r := runner(emptyQstat, acct)
		_, lines := runSacct(r, "--name=sx-hrt", "-o", "JobID,JobName", "-P", "-n")
		Expect(lines).To(Equal([]string{"1380|sx-hrt"}))
		Expect(qacctArgsOf(r)).To(Equal([][]string{{"-j", "sx-hrt", "-o", "gridware"}}))
	})

	It("filters an id selection and live jobs by name", func() {
		q := qstatXML(jobList("4711", "r", "all.q@node01", "")) // live job is named "job"
		acct := map[string]string{"1380": fixture("qacct_1380_hrt_timeout.txt")}
		_, lines := runSacct(runner(q, acct), "--name", "sx-hrt,other", "-o", "JobID", "-P", "-n", "-j", "1380,4711")
		Expect(lines).To(Equal([]string{"1380"}))
	})

	DescribeTable("refuses a name or user qacct would read as an option",
		func(args ...string) {
			r := runner(emptyQstat, nil)
			var errBuf bytes.Buffer
			Expect(runWith(r, "mem_free", args, io.Discard, &errBuf)).To(Equal(1))
			Expect(errBuf.String()).To(ContainSubstring("invalid job or user name"))
			Expect(qacctArgsOf(r)).To(BeEmpty())
		},
		Entry("--name=-f", "--name=-f"),
		Entry("-u -o", "-u", "-o"),
	)
})

var _ = Describe("sacct -a/--allusers", func() {
	window := []string{"-S", "2026-10-12", "-o", "JobID", "-P"}

	It("narrows a listing to the caller's own jobs by default", func() {
		asUser("alice", false)
		r := runner(emptyQstat, nil)
		runSacct(r, window...)
		Expect(qacctArgsOf(r)[0]).To(ContainElements("-o", "alice"))
	})

	It("lists every user with -a", func() {
		asUser("alice", false)
		for _, flag := range []string{"-a", "--allusers"} {
			r := runner(emptyQstat, nil)
			runSacct(r, append([]string{flag}, window...)...)
			Expect(qacctArgsOf(r)[0]).NotTo(ContainElement("-o"), flag)
		}
	})

	It("lists every user for root, as SLURM does", func() {
		asUser("root", true)
		r := runner(emptyQstat, nil)
		runSacct(r, window...)
		Expect(qacctArgsOf(r)[0]).NotTo(ContainElement("-o"))
	})

	It("leaves an id selection unnarrowed", func() {
		asUser("alice", false)
		r := runner(emptyQstat, nil)
		runSacct(r, "-j", "4711")
		Expect(qacctArgsOf(r)).To(Equal([][]string{{"-j", "4711"}}))
	})

	It("narrows live jobs too", func() {
		asUser("alice", false)
		q := qstatXML(jobList("4711", "r", "all.q@node01", "")) // owned by alice
		_, lines := runSacct(runner(q, nil), window...)
		Expect(lines).To(ContainElement("4711"))
		asUser("bob", false)
		_, lines = runSacct(runner(q, nil), window...)
		Expect(lines).NotTo(ContainElement("4711"))
	})
})

var _ = Describe("sacct column selection: -b, SACCT_FORMAT, --delimiter", func() {
	setEnv := func(v string) {
		old := getenv
		getenv = func(k string) string {
			if k == "SACCT_FORMAT" {
				return v
			}
			return ""
		}
		DeferCleanup(func() { getenv = old })
	}

	DescribeTable("builds the header the way SLURM does",
		func(env string, args []string, header string) {
			setEnv(env)
			_, lines := runSacct(oneJob("4711"), append(args, "-P", "-j", "4711")...)
			Expect(lines[0]).To(Equal(header))
		},
		Entry("-b", "", []string{"-b"}, "JobID|State|ExitCode"),
		Entry("--brief", "", []string{"--brief"}, "JobID|State|ExitCode"),
		Entry("-o then -b appends the brief set", "", []string{"-o", "JobName", "-b"}, "JobName|JobID|State|ExitCode"),
		Entry("repeated -o accumulate", "", []string{"-o", "JobID", "-o", "User"}, "JobID|User"),
		Entry("SACCT_FORMAT is the default", "JobID,User", nil, "JobID|User"),
		Entry("-o beats SACCT_FORMAT", "JobID,User", []string{"-o", "State"}, "State"),
		Entry("-b beats SACCT_FORMAT", "JobID,User", []string{"-b"}, "JobID|State|ExitCode"),
		Entry("an empty SACCT_FORMAT is ignored", "  ", nil, "JobID|JobName|Partition|Account|AllocCPUS|State|ExitCode"),
	)

	It("joins -p/-P output with --delimiter", func() {
		_, lines := runSacct(oneJob("4711"), "-o", "JobID,State", "--delimiter=,", "-p", "-j", "4711")
		Expect(lines).To(Equal([]string{"JobID,State,", "4711,COMPLETED,"}))
		_, lines = runSacct(oneJob("4711"), "-o", "JobID,State", "--delimiter", "::", "-P", "-j", "4711")
		Expect(lines).To(Equal([]string{"JobID::State", "4711::COMPLETED"}))
	})

	It("ignores --delimiter for the table, as SLURM does", func() {
		_, with := runSacct(oneJob("4711"), "--delimiter=,", "-j", "4711")
		_, without := runSacct(oneJob("4711"), "-j", "4711")
		Expect(with).To(Equal(without))
	})
})
