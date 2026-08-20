package sacct

import (
	"bytes"
	"io"
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
		_, lines := runSacct(runner(emptyQstat, acct), "--parsable2", "-j", "4712_2")
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
		_, lines := runSacct(runner(emptyQstat, acct), "--parsable2", "-j", "4711,4713")
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
		_, lines := runSacct(runner(emptyQstat, acct), "-n", "-j", "4711")
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
