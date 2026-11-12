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

package sacct

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// A job Grid Engine is deleting (qstat dr, dt, ...) is still in qstat for a few
// seconds, longer when its node does not answer. Grid Engine does not say why
// it is being deleted, so sacct reports no terminal state before the accounting
// record exists: a poller that latches the first terminal state (submitit)
// would keep CANCELLED for a job that really ran into its time limit.
// COMPLETING is ruled out as well: dagster-slurm reads every state starting
// with "COMPLET" as COMPLETED, which turned a cancelled job into a successful
// Dagster run.
var _ = Describe("sacct for a job that is being deleted", func() {
	deleting := func(state string) *fake.Runner {
		return runner(qstatXML(jobList("4711", state, "all.q@node01", "")), nil)
	}

	DescribeTable("reports it as RUNNING",
		func(geState string) {
			_, lines := runSacct(deleting(geState), "-o", "JobID,State,NodeList", "--parsable2", "-j", "4711")
			Expect(lines).To(Equal([]string{"JobID|State|NodeList", "4711|RUNNING|node01"}))
		},
		Entry("deleted while running", "dr"),
		Entry("deleted while transferring", "dt"),
		Entry("deleted while rescheduled", "dRr"),
		Entry("deleted while suspended", "ds"),
		Entry("deleted while pending", "dqw"),
		Entry("deleted while in error state", "dEqw"),
	)

	It("answers dagster-slurm's state query with RUNNING", func() {
		_, lines := runSacct(deleting("dr"), "-X", "-n", "-j", "4711", "-o", "State%20")
		Expect(lines).To(HaveLen(1))
		Expect(strings.TrimSpace(lines[0])).To(Equal("RUNNING"))
	})

	It("answers dagster-slurm's final accounting query with RUNNING in the State column", func() {
		_, lines := runSacct(deleting("dr"), "-j", "4711", "-n", "-P",
			"--format=JobID,Elapsed,TotalCPU,MaxRSS,AllocNodes,AllocCPUS,State,ExitCode")
		Expect(lines).To(HaveLen(1))
		fields := strings.Split(lines[0], "|")
		Expect(fields).To(HaveLen(8))
		Expect(fields[0]).To(Equal("4711"))
		Expect(fields[6]).To(Equal("RUNNING"))
	})

	It("is selected by -s R and by -s RUNNING", func() {
		for _, state := range []string{"R", "RUNNING"} {
			_, lines := runSacct(deleting("dr"), "-o", "JobID,State", "--parsable2", "-s", state, "-j", "4711")
			Expect(lines).To(ContainElement("4711|RUNNING"), state)
		}
	})

	DescribeTable("is not selected by a state it is not in",
		func(state string) {
			_, lines := runSacct(deleting("dr"), "-o", "JobID,State", "--parsable2", "-s", state, "-j", "4711")
			Expect(lines).To(Equal([]string{"JobID|State"}))
		},
		Entry("COMPLETING, which sacct never reports", "CG"),
		Entry("CANCELLED, which only the accounting record can say", "CA"),
		Entry("TIMEOUT", "TO"),
		Entry("COMPLETED", "CD"),
	)

	It("wins over an accounting record of an earlier, clean run of the same id", func() {
		acct := map[string]string{"4711": acctRecord("4711", "undefined", "0", "0")}
		r := runner(qstatXML(jobList("4711", "dr", "all.q@node01", "")), acct)
		_, lines := runSacct(r, "-o", "JobID,State", "--parsable2", "-j", "4711")
		Expect(lines).To(ContainElement("4711|RUNNING"))
		Expect(lines).NotTo(ContainElement("4711|COMPLETED"))
	})

	It("reports a deleting array element 0-based", func() {
		r := runner(qstatXML(jobList("4712", "dr", "all.q@node01", "1")), nil)
		_, lines := runSacct(r, "-o", "JobID,State", "--parsable2", "-j", "4712")
		Expect(lines).To(ContainElement("4712_0|RUNNING"))
	})

	// A poller such as submitit keeps the first terminal state it sees, so a
	// job still in qstat must never carry one: only qacct knows the outcome.
	DescribeTable("never reports a terminal state for a job still in qstat",
		func(geState string) {
			_, lines := runSacct(deleting(geState), "-X", "-n", "-P", "-j", "4711", "-o", "State")
			Expect(lines).To(HaveLen(1))
			Expect(lines[0]).To(BeElementOf("RUNNING", "PENDING"), "GE state %q", geState)
		},
		Entry("running", "r"), Entry("transferring", "t"), Entry("pending", "qw"),
		Entry("held", "hqw"), Entry("error", "Eqw"), Entry("suspended", "s"),
		Entry("rescheduled", "Rr"), Entry("deleting", "dr"), Entry("deleting in transfer", "dt"),
		Entry("deleting while suspended", "ds"), Entry("deleting while pending", "dqw"),
		Entry("deleting in error state", "dEqw"), Entry("deleting while rescheduled", "dRr"),
	)

	// The property dagster-slurm depends on, whatever a live state is called:
	// a job still in qstat must never read as finished successfully.
	DescribeTable("never reports a live job with a state that starts with COMPLET",
		func(geState string) {
			_, lines := runSacct(deleting(geState), "-X", "-n", "-P", "-j", "4711", "-o", "State")
			Expect(lines).To(HaveLen(1))
			Expect(lines[0]).NotTo(BeEmpty())
			Expect(lines[0]).NotTo(HavePrefix("COMPLET"), "GE state %q", geState)
		},
		Entry("running", "r"), Entry("transferring", "t"), Entry("pending", "qw"),
		Entry("held", "hqw"), Entry("error", "Eqw"), Entry("suspended", "s"),
		Entry("rescheduled", "Rr"), Entry("deleting", "dr"), Entry("deleting in transfer", "dt"),
		Entry("deleting while suspended", "ds"), Entry("deleting while pending", "dqw"),
	)
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

	// At its limit a parallel job is either killed (failed=100) or, when its
	// tasks die first, leaves by itself with the script's exit status and
	// failed=0. Which one is a race (CS-3087); both ran into the limit.
	Describe("a parallel job whose tasks were killed at the limit before the job script", func() {
		state := func(acct, id string) []string {
			r := runner(emptyQstat, map[string]string{id: acct})
			_, lines := runSacct(r, "-o", "JobID,State,ExitCode,Elapsed,Timelimit", "-P", "-n", "-j", id)
			return lines
		}

		It("is TIMEOUT from the one record of accounting_summary TRUE", func() {
			Expect(state(fixture("qacct_47_pe_tasks_hit_hrt_summary.txt"), "47")).
				To(Equal([]string{"47|TIMEOUT|1:0|00:01:00|00:01:00"}))
		})

		It("is TIMEOUT with the task records of accounting_summary FALSE", func() {
			Expect(state(fixture("qacct_54_pe_tasks_hit_hrt.txt"), "54")).
				To(Equal([]string{"54|TIMEOUT|1:0|00:01:00|00:01:00"}))
		})

		It("is selected by -s TO and not by -s F", func() {
			r := runner(emptyQstat, map[string]string{"47": fixture("qacct_47_pe_tasks_hit_hrt_summary.txt")})
			_, lines := runSacct(r, "-o", "JobID,State", "-P", "-n", "-s", "TO", "-j", "47")
			Expect(lines).To(Equal([]string{"47|TIMEOUT"}))
			_, lines = runSacct(r, "-o", "JobID,State", "-P", "-n", "-s", "F", "-j", "47")
			Expect(lines).NotTo(ContainElement(ContainSubstring("47")))
		})

		It("stays FAILED when the job failed before its limit", func() {
			Expect(state(fixture("qacct_52_pe_failed_before_hrt.txt"), "52")).
				To(Equal([]string{"52|FAILED|3:0|00:00:07|00:01:00"}))
		})

		It("stays FAILED one second short of the limit", func() {
			acct := strings.ReplaceAll(fixture("qacct_47_pe_tasks_hit_hrt_summary.txt"), "-l h_rt=60 ", "-l h_rt=61 ")
			Expect(state(acct, "47")).To(Equal([]string{"47|FAILED|1:0|00:01:00|00:01:01"}))
		})

		It("stays FAILED without a time limit on the command line", func() {
			acct := strings.ReplaceAll(fixture("qacct_47_pe_tasks_hit_hrt_summary.txt"), "-l h_rt=60 ", "")
			Expect(state(acct, "47")).To(Equal([]string{"47|FAILED|1:0|00:01:00|"}))
		})

		It("stays COMPLETED when the job finished cleanly as its limit passed", func() {
			acct := strings.ReplaceAll(fixture("qacct_47_pe_tasks_hit_hrt_summary.txt"),
				"exit_status                        1 ", "exit_status                        0 ")
			Expect(state(acct, "47")).To(Equal([]string{"47|COMPLETED|0:0|00:01:00|00:01:00"}))
		})

		It("stays FAILED for a serial job, which has no tasks to lose", func() {
			acct := fixture("qacct_1380_hrt_timeout.txt")
			acct = strings.ReplaceAll(acct, "failed                             100 : assumedly after job", "failed                             0    ")
			acct = strings.ReplaceAll(acct, "exit_status                        137", "exit_status                        1  ")
			Expect(state(acct, "1380")).To(Equal([]string{"1380|FAILED|1:0|00:00:06|00:00:05"}))
		})
	})

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
		_, lines := runSacct(runner(q, nil), "-o", "JobID,ReqMem,Timelimit,WorkDir", "-P", "-n", "-j", "4711")
		Expect(lines).To(Equal([]string{"4711|||"}))
	})
})

// The plain qstat listing names only a job's master queue instance. SLURM
// always prints a node count, and dagster-slurm turns AllocNodes into node
// hours, so a running job's hosts are read from qstat -j when the column is
// asked for.
var _ = Describe("sacct NNodes of a live job", func() {
	// live answers the listing with one job and `qstat -xml -j` with detail.
	live := func(list, detail string) *fake.Runner {
		return &fake.Runner{Responder: func(name string, args []string) fake.Response {
			if name == "qstat" && slices.Contains(args, "-j") {
				if detail == "" {
					return fake.Response{Exit: 1}
				}
				return fake.Response{Stdout: []byte(detail)}
			}
			if name == "qstat" {
				return fake.Response{Stdout: []byte(list)}
			}
			return fake.Response{}
		}}
	}
	detailCalls := func(r *fake.Runner) [][]string {
		var out [][]string
		for _, c := range r.Calls {
			if c.Name == "qstat" && slices.Contains(c.Args, "-j") {
				out = append(out, c.Args)
			}
		}
		return out
	}
	twoHosts := func() string {
		b, err := os.ReadFile(filepath.Join("..", "..", "gedata", "testdata", "qstat_j_signal_multinode_9.1.6.xml"))
		Expect(err).NotTo(HaveOccurred())
		return string(b)
	}
	multiSlot := func(id, state string) string {
		return strings.ReplaceAll(jobList(id, state, "all.q@ocs-master", ""), "<slots>1</slots>", "<slots>2</slots>")
	}

	DescribeTable("counts the granted hosts of a running job (live OCS 9.1.6 detail)",
		func(field string) {
			r := live(qstatXML(multiSlot("1400", "r")), twoHosts())
			_, lines := runSacct(r, "-o", "JobID,"+field+",State", "-P", "-n", "-j", "1400")
			Expect(lines).To(Equal([]string{"1400|2|RUNNING"}))
			Expect(detailCalls(r)).To(Equal([][]string{{"-xml", "-j", "1400"}}))
		},
		Entry("NNodes", "NNodes"),
		Entry("AllocNodes, which dagster-slurm asks for", "AllocNodes"),
	)

	It("counts them for a job that is being removed", func() {
		r := live(qstatXML(multiSlot("1400", "dr")), twoHosts())
		_, lines := runSacct(r, "-o", "JobID,NNodes", "-P", "-n", "-j", "1400")
		Expect(lines).To(Equal([]string{"1400|2"}))
	})

	It("asks for every job at once when several running jobs are reported", func() {
		list := qstatXML(multiSlot("1400", "r") + jobList("1401", "r", "all.q@ocs-master", ""))
		r := live(list, twoHosts())
		_, lines := runSacct(r, "-o", "JobID,NNodes", "-P", "-n", "-j", "1400,1401")
		Expect(lines).To(Equal([]string{"1400|2", "1401|1"}))
		Expect(detailCalls(r)).To(Equal([][]string{{"-xml", "-j", "*"}}))
	})

	It("says 1 for a single-slot job when the hosts cannot be read", func() {
		r := live(qstatXML(jobList("4711", "r", "all.q@node01", "")), "")
		code, lines := runSacct(r, "-o", "JobID,NNodes", "-P", "-n", "-j", "4711")
		Expect(code).To(Equal(0))
		Expect(lines).To(Equal([]string{"4711|1"}))
	})

	It("leaves it empty for a multi-slot job when the hosts cannot be read", func() {
		r := live(qstatXML(multiSlot("1400", "r")), "")
		code, lines := runSacct(r, "-o", "JobID,NNodes", "-P", "-n", "-j", "1400")
		Expect(code).To(Equal(0))
		Expect(lines).To(Equal([]string{"1400|"}))
	})

	It("leaves it empty for a pending job and does not ask", func() {
		r := live(qstatXML(jobList("4711", "qw", "", "")), twoHosts())
		_, lines := runSacct(r, "-o", "JobID,NNodes", "-P", "-n", "-j", "4711")
		Expect(lines).To(Equal([]string{"4711|"}))
		Expect(detailCalls(r)).To(BeEmpty())
	})

	It("does not ask when the column is not in the format", func() {
		r := live(qstatXML(multiSlot("1400", "r")), twoHosts())
		runSacct(r, "-o", "JobID,State", "-P", "-n", "-j", "1400")
		Expect(detailCalls(r)).To(BeEmpty())
	})

	It("does not ask for a running job that is not in the report", func() {
		r := live(qstatXML(multiSlot("1400", "r")), twoHosts())
		runSacct(r, "-o", "JobID,NNodes", "-P", "-n", "-j", "4711")
		Expect(detailCalls(r)).To(BeEmpty())
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
