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

package sbatch

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

const acctSep = "=============================================================="

// record is one qacct job record.
func record(task string, failed, exit int) string {
	return fmt.Sprintf("%s\njobnumber 4711\ntaskid %s\nfailed %d\nexit_status %d\n", acctSep, task, failed, exit)
}

// cluster answers qstat -j "present" for the first polls, then "gone", and
// qacct with the given records.
func cluster(polls int, acct string) *fake.Runner {
	n := 0
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		switch name {
		case "qstat":
			if n++; n <= polls {
				return fake.Response{}
			}
			return fake.Response{Exit: 1, Stdout: []byte("Following jobs do not exist: \n4711\n")}
		case "qacct":
			if acct == "" {
				return fake.Response{Exit: 1, Stderr: []byte("error: job id 4711 not found")}
			}
			return fake.Response{Stdout: []byte(acct)}
		}
		return fake.Response{}
	}}
}

var _ = Describe("sbatch --wait", func() {
	BeforeEach(func() {
		first, most, acct, errs := waitFirstPoll, waitMaxPoll, waitAcctFor, waitErrorsFor
		waitFirstPoll, waitMaxPoll, waitAcctFor = time.Millisecond, 2*time.Millisecond, 50*time.Millisecond
		waitErrorsFor = 50 * time.Millisecond
		DeferCleanup(func() { waitFirstPoll, waitMaxPoll, waitAcctFor, waitErrorsFor = first, most, acct, errs })
	})

	It("parses --wait and -W, and leaves the next token to the script", func() {
		for _, args := range [][]string{{"--wait", "job.sh"}, {"-W", "job.sh"}} {
			opt, _, err := parseFlags(args)
			Expect(err).NotTo(HaveOccurred())
			Expect(opt.wait).To(BeTrue())
			Expect(opt.script).To(Equal("job.sh"))
		}
	})

	DescribeTable("exits with the job's exit status",
		func(acct string, tasks, want int) {
			r := cluster(2, acct)
			Expect(waitForJob(r, "4711", tasks, &bytes.Buffer{}, nil)).To(Equal(want))
		},
		Entry("success", record("undefined", 0, 0), 1, 0),
		Entry("exit 3", record("undefined", 0, 3), 1, 3),
		Entry("killed by a signal: GE's 128+signal", record("undefined", 100, 137), 1, 137),
		Entry("failed by Grid Engine with no status of its own", record("undefined", 26, 0), 1, 1),
		Entry("array: the highest over its tasks", record("1", 0, 0)+record("2", 0, 5)+record("3", 0, 2), 3, 5),
	)

	It("keeps polling until every array task has its record", func() {
		calls := 0
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			if name == "qstat" {
				return fake.Response{Exit: 1, Stdout: []byte("Following jobs do not exist:\n")}
			}
			if name == "qacct" {
				if calls++; calls == 1 {
					return fake.Response{Stdout: []byte(record("1", 0, 0))}
				}
				return fake.Response{Stdout: []byte(record("1", 0, 0) + record("2", 0, 4))}
			}
			return fake.Response{}
		}}
		Expect(waitForJob(r, "4711", 2, &bytes.Buffer{}, nil)).To(Equal(4))
		Expect(calls).To(Equal(2))
	})

	It("fails rather than report success when no accounting record appears", func() {
		var errBuf bytes.Buffer
		Expect(waitForJob(cluster(0, ""), "4711", 1, &errBuf, nil)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("exit status unknown"))
	})

	It("stops waiting on an interrupt and leaves the job alone", func() {
		r := cluster(1000000, "")
		interrupt := make(chan os.Signal, 1)
		interrupt <- syscall.SIGINT
		var errBuf bytes.Buffer
		Expect(waitForJob(r, "4711", 1, &errBuf, interrupt)).To(Equal(128 + 2))
		Expect(errBuf.String()).To(ContainSubstring("job 4711 keeps running"))
		for _, c := range r.Calls {
			Expect(c.Name).NotTo(Equal("qdel"))
		}
	})

	It("warns that an OCS older than the accounting fix cannot report a failure", func() {
		r := cluster(0, record("undefined", 0, 0))
		inner := r.Responder
		r.Responder = func(name string, args []string) fake.Response {
			if name == "qconf" {
				return fake.Response{Stdout: []byte("OCS 9.0.10 (250101-0000)\n")}
			}
			return inner(name, args)
		}
		var errBuf bytes.Buffer
		Expect(waitForJob(r, "4711", 1, &errBuf, nil)).To(Equal(0))
		Expect(strings.ToLower(errBuf.String())).To(ContainSubstring("cannot see a failure"))
	})

	It("keeps waiting through a qmaster outage and still returns the job's status", func() {
		qstats := 0
		r := cluster(0, record("undefined", 0, 3))
		inner := r.Responder
		r.Responder = func(name string, args []string) fake.Response {
			if name == "qstat" {
				if qstats++; qstats <= 3 {
					return fake.Response{Exit: 1, Stderr: []byte("error: unable to contact qmaster")}
				}
			}
			return inner(name, args)
		}
		var errBuf bytes.Buffer
		Expect(waitForJob(r, "4711", 1, &errBuf, nil)).To(Equal(3))
		Expect(strings.Count(errBuf.String(), "still waiting")).To(Equal(1), "one warning per outage")
	})

	It("tolerates a failing qacct the same way", func() {
		qaccts := 0
		r := cluster(0, record("undefined", 0, 4))
		inner := r.Responder
		r.Responder = func(name string, args []string) fake.Response {
			if name == "qacct" {
				if qaccts++; qaccts <= 2 {
					return fake.Response{Exit: 1, Stderr: []byte("error: unable to read accounting file")}
				}
			}
			return inner(name, args)
		}
		Expect(waitForJob(r, "4711", 1, &bytes.Buffer{}, nil)).To(Equal(4))
	})

	It("gives up once qstat has kept failing for waitErrorsFor", func() {
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			if name == "qstat" {
				return fake.Response{Exit: 1, Stderr: []byte("error: unable to contact qmaster")}
			}
			return fake.Response{}
		}}
		var errBuf bytes.Buffer
		Expect(waitForJob(r, "4711", 1, &errBuf, nil)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("keeps failing"))
		Expect(errBuf.String()).To(ContainSubstring("may still be running"))
	})

	It("counts only the last run of a rescheduled task", func() {
		run := func(end string, exit int) string {
			return fmt.Sprintf("%s\njobnumber 4711\ntaskid undefined\nend_time %s\nfailed 0\nexit_status %d\n",
				acctSep, end, exit)
		}
		// The earlier run comes first, so taking the first record would give 9.
		acct := run("2026-08-20 19:40:00.000000", 9) + run("2026-08-20 19:50:00.000000", 0)
		Expect(waitForJob(cluster(0, acct), "4711", 1, &bytes.Buffer{}, nil)).To(Equal(0))
	})

	It("stops waiting for the accounting record on an interrupt", func() {
		interrupt := make(chan os.Signal, 1)
		r := cluster(0, "")
		inner := r.Responder
		r.Responder = func(name string, args []string) fake.Response {
			if name == "qacct" {
				select {
				case interrupt <- syscall.SIGTERM:
				default:
				}
			}
			return inner(name, args)
		}
		var errBuf bytes.Buffer
		Expect(waitForJob(r, "4711", 1, &errBuf, interrupt)).To(Equal(128 + 15))
		Expect(errBuf.String()).To(ContainSubstring("stopped waiting for the accounting record"))
	})

	Describe("wired into sbatch", func() {
		// submitAndWait submits a job through run() with --wait against a
		// cluster that has already finished it with the given records.
		var lastErr string
		submitAndWait := func(acct string, args ...string) (int, string, *fake.Runner) {
			var captured []string
			r := parRunner(parUsage, &captured)
			submit := r.Responder
			r.Responder = func(name string, a []string) fake.Response {
				switch name {
				case "qstat":
					return fake.Response{Exit: 1, Stdout: []byte("Following jobs do not exist:\n4711\n")}
				case "qacct":
					return fake.Response{Stdout: []byte(acct)}
				}
				return submit(name, a)
			}
			script := filepath.Join(GinkgoT().TempDir(), "job.sh")
			Expect(os.WriteFile(script, []byte("#!/bin/bash\n"), 0o700)).To(Succeed())
			var out, errOut bytes.Buffer
			code := run(r, testCfg(), "/usr/bin/slurm-shim", append(append([]string{"-p", "batch"}, args...), script), &out, &errOut)
			lastErr = errOut.String()
			return code, out.String(), r
		}

		It("prints the job id, then exits with the job's status", func() {
			code, out, r := submitAndWait(record("undefined", 0, 3), "--wait")
			Expect(code).To(Equal(3))
			Expect(out).To(Equal("Submitted batch job 4711\n"))
			var names []string
			for _, c := range r.Calls {
				names = append(names, c.Name)
			}
			Expect(names).To(ContainElements("qsub", "qstat", "qacct"))
		})

		It("keeps --parsable output to the bare job id", func() {
			code, out, _ := submitAndWait(record("undefined", 0, 0), "--parsable", "-W")
			Expect(code).To(Equal(0))
			Expect(out).To(Equal("4711\n"))
		})

		It("waits for every task of a stepped array (0-10:5 is 3 tasks)", func() {
			three := record("1", 0, 0) + record("6", 0, 7) + record("11", 0, 2)
			code, _, _ := submitAndWait(three, "--wait", "--array=0-10:5")
			Expect(code).To(Equal(7))

			code, _, _ = submitAndWait(record("1", 0, 0)+record("6", 0, 0), "--wait", "--array=0-10:5")
			Expect(code).To(Equal(1), "two of three records: status unknown")
		})

		It("submits nothing and waits for nothing in a dry run, but says it would wait", func() {
			GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
			code, _, r := submitAndWait("", "--wait")
			Expect(code).To(Equal(0))
			Expect(lastErr).To(ContainSubstring("wait for the job to end"))
			for _, c := range r.Calls {
				Expect(c.Name).NotTo(BeElementOf("qstat", "qacct"))
			}
		})
	})
})
