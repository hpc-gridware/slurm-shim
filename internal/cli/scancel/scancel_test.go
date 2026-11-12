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

package scancel

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func TestScancel(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Scancel Suite")
}

var _ = Describe("scancel [REQ-SCL-001]", func() {
	It("maps a bare job id to qdel", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"4711"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls).To(HaveLen(1))
		Expect(r.Calls[0].Name).To(Equal("qdel"))
		Expect(r.Calls[0].Args).To(Equal([]string{"4711"}))
	})

	It("maps a 0-based array element to the 1-based GE task (job id before -t)", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"4711_2"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls[0].Args).To(Equal([]string{"4711", "-t", "3"}))
	})

	It("maps array element 0 to GE task 1 (submitit's scancel N_0)", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"4711_0"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls[0].Args).To(Equal([]string{"4711", "-t", "1"}))
	})

	It("passes -u through to qdel -u", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"-u", "alice"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls[0].Args).To(Equal([]string{"-u", "alice"}))
	})

	It("cancels multiple job ids", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"4711", "4712"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls).To(HaveLen(2))
	})

	It("maps --signal (submitit checkpoint-preempt) to qmod -rj reschedule", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"--signal=USR2", "4711"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls[0].Name).To(Equal("qmod"))
		Expect(r.Calls[0].Args).To(Equal([]string{"-rj", "4711"}))
	})

	It("reschedules a single array element (--signal N_k -> qmod -rj N.<k+1>)", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"-s", "USR2", "4711_0"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls[0].Args).To(Equal([]string{"-rj", "4711.1"}))
	})

	It("treats a repeated reschedule of a finished job as non-fatal (submitit sends two)", func() {
		r := &fake.Runner{Responder: func(name string, _ []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("job 4711 does not exist")}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"--signal=USR2", "4711"}, io.Discard, &errBuf)).To(Equal(0))
		Expect(errBuf.String()).To(ContainSubstring("does not exist"))
	})

	It("surfaces a qdel failure exit code", func() {
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("alice - you do not have the necessary privileges to delete the job \"4711\"")}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"4711"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("scancel: error: alice - you do not have the necessary privileges"))
	})

	// SLURM's scancel exits 0 and prints nothing for a job that has already
	// finished or never existed (scancel.c ignores ESLURM_ALREADY_DONE and
	// ESLURM_INVALID_JOB_ID when it sends SIGKILL). qdel says the same thing
	// for both; the text is what OCS 9.1.6 prints.
	DescribeTable("treats a job that is already gone as cancelled",
		func(said string, onStderr bool, wantQdel []string, args ...string) {
			r := &fake.Runner{Responder: func(string, []string) fake.Response {
				if onStderr {
					return fake.Response{Exit: 1, Stderr: []byte(said)}
				}
				// On stdout, which is where qdel puts it.
				return fake.Response{Exit: 1, Stdout: []byte(said)}
			}}
			var errBuf bytes.Buffer
			Expect(run(r, args, io.Discard, &errBuf)).To(Equal(0))
			Expect(errBuf.String()).To(BeEmpty())
			Expect(r.Calls).To(HaveLen(1))
			Expect(r.Calls[0].Name).To(Equal("qdel"))
			Expect(r.Calls[0].Args).To(Equal(wantQdel))
		},
		Entry("plain cancel", gone, false, []string{"4711"}, "4711"),
		Entry("the message on stderr instead", gone, true, []string{"4711"}, "4711"),
		Entry("array element", gone, false, []string{"4711", "-t", "1"}, "4711_0"),
		Entry("array element whose task is gone", "job \"4711\" task \"1\" does not exist\n",
			false, []string{"4711", "-t", "1"}, "4711_0"),
		Entry("--signal=KILL", gone, false, []string{"4711"}, "--signal=KILL", "4711"),
		Entry("several lines, each one a job that is gone",
			gone+"denied: job \"4712\" does not exist\n", false, []string{"4711"}, "4711"),
	)

	// The streams are joined line by line. Run together, stderr without a
	// trailing newline fused with stdout into ONE line containing "does not
	// exist", and a real error was reported as a cancelled job.
	DescribeTable("keeps a real error next to a \"does not exist\" line an error",
		func(resp fake.Response, want ...string) {
			r := &fake.Runner{Responder: func(string, []string) fake.Response { return resp }}
			var errBuf bytes.Buffer
			Expect(run(r, []string{"4711"}, io.Discard, &errBuf)).To(Equal(1))
			for _, w := range want {
				Expect(errBuf.String()).To(ContainSubstring(w))
			}
			Expect(strings.Split(strings.TrimSpace(errBuf.String()), "\n")).To(HaveLen(2),
				"both messages are shown, each on its own line")
		},
		Entry("stderr without a trailing newline, stdout gone",
			fake.Response{Exit: 1, Stderr: []byte("error: commlib error: got select error"), Stdout: []byte(gone)},
			"commlib error", "does not exist"),
		Entry("stderr with a trailing newline, stdout gone",
			fake.Response{Exit: 1, Stderr: []byte("error: commlib error: got select error\n"), Stdout: []byte(gone)},
			"commlib error", "does not exist"),
		Entry("two lines on stdout, only one about a missing job",
			fake.Response{Exit: 1, Stdout: []byte(gone + "error: commlib error: can't connect to service\n")},
			"can't connect", "does not exist"),
	)

	// Only qdel's own words can mean "gone". The operating system saying a FILE
	// does not exist means qdel never ran.
	DescribeTable("reports a qdel that could not be started",
		func(spawnErr error) {
			r := &fake.Runner{Responder: func(string, []string) fake.Response {
				return fake.Response{Exit: -1, Err: spawnErr}
			}}
			var errBuf bytes.Buffer
			Expect(run(r, []string{"4711"}, io.Discard, &errBuf)).To(Equal(1))
			Expect(errBuf.String()).To(HavePrefix("scancel: error: running qdel: "))
		},
		Entry("not on PATH", errors.New(`exec: "qdel": executable file not found in $PATH`)),
		Entry("an error that reads like a missing job", errors.New(`job "qdel" does not exist`)),
	)

	// qdel reads a non-numeric argument as a job NAME or pattern, so these must
	// never reach it: a typo would come back "does not exist" and pass as a
	// cancelled job, and a name or "*" would delete jobs nobody named by id.
	DescribeTable("refuses an argument that is not a job id, without calling qdel",
		func(arg string) {
			r := &fake.Runner{Responder: func(string, []string) fake.Response {
				return fake.Response{Exit: 1, Stdout: []byte("The job x of user(s) alice does not exist\n")}
			}}
			var errBuf bytes.Buffer
			Expect(run(r, []string{arg}, io.Discard, &errBuf)).To(Equal(1))
			Expect(errBuf.String()).To(Equal("scancel: error: Invalid job id " + arg + "\n"))
			Expect(r.Calls).To(BeEmpty())
		},
		Entry("a typo", "47l1"),
		Entry("a job name", "train"),
		Entry("a pattern", "*"),
		Entry("a step id", "4711.batch"),
		Entry("a negative-looking suffix", "4711_"),
		Entry("an empty array range", "4711_[]"),
	)

	It("refuses the whole command when one id of several is invalid", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"4711", "trian"}, io.Discard, io.Discard)).To(Equal(1))
		Expect(r.Calls).To(BeEmpty())
	})

	DescribeTable("accepts every form of a job id",
		func(arg string, want ...string) {
			r := &fake.Runner{}
			Expect(run(r, []string{arg}, io.Discard, io.Discard)).To(Equal(0))
			Expect(r.Calls).To(HaveLen(1))
			Expect(r.Calls[0].Args).To(Equal(want))
		},
		Entry("a job", "4711", "4711"),
		Entry("an array element", "4711_2", "4711", "-t", "3"),
		Entry("an array range", "4711_[0-3]", "4711", "-t", "[0-3]"),
	)

	It("cancels a comma-separated list one job at a time", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"4711,4712"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls).To(HaveLen(2))
		Expect(r.Calls[0].Args).To(Equal([]string{"4711"}))
		Expect(r.Calls[1].Args).To(Equal([]string{"4712"}))
	})

	It("does not take an unrelated \"does not exist\" for a job that is gone", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stdout: []byte("denied: user \"alice\" does not exist in access list\n")}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"4711"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("access list"))
	})

	It("keeps a failing qdel that says nothing an error, with qdel's exit status", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 3}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"4711"}, io.Discard, &errBuf)).To(Equal(3))
		Expect(errBuf.String()).To(BeEmpty())
	})

	// submitit sends two signals in a row; the second finds the job gone. qmod
	// says so on stdout.
	It("warns, and does not fail, when a reschedule is refused on stdout", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stdout: []byte("invalid queue or job \"4711\"\n")}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"--signal=USR2", "4711"}, io.Discard, &errBuf)).To(Equal(0))
		Expect(errBuf.String()).To(Equal("scancel: warning: invalid queue or job \"4711\"\n"))
	})

	It("reports a qmod that could not be started", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: -1, Err: errors.New("exec: \"qmod\": executable file not found in $PATH")}
		}}
		for _, sig := range []string{"USR2", "STOP"} {
			var errBuf bytes.Buffer
			Expect(run(r, []string{"--signal=" + sig, "4711"}, io.Discard, &errBuf)).To(Equal(1))
			Expect(errBuf.String()).To(HavePrefix("scancel: error: running qmod: "))
		}
	})

	It("still fails when one of several jobs cannot be cancelled", func() {
		r := &fake.Runner{Responder: func(_ string, args []string) fake.Response {
			if args[0] == "4712" {
				return fake.Response{Exit: 1, Stderr: []byte("alice - you do not have the necessary privileges to delete the job \"4712\"")}
			}
			return fake.Response{Exit: 1, Stderr: []byte("denied: job \"4711\" does not exist")}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"4711", "4712"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("4712"))
		Expect(errBuf.String()).NotTo(ContainSubstring("does not exist"))
	})

	It("keeps a missing job an error for suspend and resume", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stdout: []byte("invalid queue or job \"4711\"")}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"--signal=STOP", "4711"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("invalid queue or job"))
	})

	It("errors when no job id is given", func() {
		Expect(run(&fake.Runner{}, nil, io.Discard, io.Discard)).To(Equal(1))
	})
})

var _ = Describe("scancel dry run [SLURM_SHIM_DRY_RUN]", func() {
	// Drives the same wrapping Run() installs, so a spec fails if the wiring is
	// removed or pointed at the wrong stream.
	dry := func(args ...string) (*fake.Runner, string, int) {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		inner := &fake.Runner{}
		var errBuf bytes.Buffer
		code := run(dryrun.Wrap(inner, &errBuf, "scancel"), args, io.Discard, &errBuf)
		return inner, errBuf.String(), code
	}

	It("reports the qdel for a whole job and cancels nothing", func() {
		inner, out, code := dry("4711")
		Expect(code).To(Equal(0))
		Expect(inner.Calls).To(BeEmpty(), "a dry run must not reach the cluster")
		Expect(out).To(ContainSubstring("would run: qdel 4711"))
	})

	// The 0-based array mapping is the part most worth previewing, since element
	// k becomes GE task k+1.
	It("reports the array-element mapping", func() {
		inner, out, code := dry("4711_2")
		Expect(code).To(Equal(0))
		Expect(inner.Calls).To(BeEmpty())
		Expect(out).To(ContainSubstring("would run: qdel 4711 -t 3"))
	})

	It("reports the reschedule behind --signal", func() {
		inner, out, code := dry("--signal", "USR2", "4711")
		Expect(code).To(Equal(0))
		Expect(inner.Calls).To(BeEmpty())
		Expect(out).To(ContainSubstring("would run: qmod -rj 4711"))
	})

	It("cancels for real when the mode is off", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "0")
		inner := &fake.Runner{}
		var errBuf bytes.Buffer
		Expect(run(dryrun.Wrap(inner, &errBuf, "scancel"), []string{"4711"}, io.Discard, &errBuf)).To(Equal(0))
		Expect(inner.Calls).To(HaveLen(1))
		Expect(inner.Calls[0].Name).To(Equal("qdel"))
	})
})

var _ = Describe("scancel options it does not translate [REQ-RUN-005]", func() {
	It("steps over their values, so a value is never cancelled as a job id", func() {
		r := &fake.Runner{}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"-A", "proj1", "-M", "cluster2", "4711"}, io.Discard, &errBuf)).To(Equal(0))
		Expect(r.Calls).To(HaveLen(1))
		Expect(r.Calls[0].Args).To(Equal([]string{"4711"}))
		Expect(errBuf.String()).To(ContainSubstring("-A ignored"))
	})
})

// gone is what qdel (OCS 9.1.6) prints, on stdout, for a job that has finished
// and for an id that never existed.
const gone = "denied: job \"4711\" does not exist\n"

// noJobs is an empty qstat -xml answer: the user has nothing queued or running.
const noJobs = "<?xml version='1.0'?><job_info><queue_info></queue_info><job_info></job_info></job_info>"

// cluster answers qstat with qstatXML and every qdel with qdelResp.
func cluster(qstatXML func(string, []string) fake.Response, qdelResp fake.Response) *fake.Runner {
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		if name == "qstat" {
			return qstatXML(name, args)
		}
		return qdelResp
	}}
}

var _ = Describe("scancel -u with nothing to cancel", func() {
	empty := func(string, []string) fake.Response { return fake.Response{Stdout: []byte(noJobs)} }
	// qdel -u fails when the user has no job. SLURM's scancel -u exits 0.
	refusal := fake.Response{Exit: 1, Stdout: []byte("There is no job registered for the following users: alice\n")}

	It("exits 0 silently when the user has no jobs, as SLURM does", func() {
		r := cluster(empty, refusal)
		var errBuf bytes.Buffer
		Expect(run(r, []string{"-u", "alice"}, io.Discard, &errBuf)).To(Equal(0))
		Expect(errBuf.String()).To(BeEmpty())
		Expect(qdels(r)).To(Equal([][]string{{"-u", "alice"}}))
	})

	It("decides that from qstat, not from qdel's wording", func() {
		r := cluster(empty, fake.Response{Exit: 1, Stdout: []byte("some other wording in a later release\n")})
		Expect(run(r, []string{"-u", "alice"}, io.Discard, io.Discard)).To(Equal(0))
	})

	It("still fails when qdel -u fails and the user does have jobs", func() {
		r := cluster(queue, fake.Response{Exit: 1, Stdout: []byte("error: commlib error: got select error\n")})
		var errBuf bytes.Buffer
		Expect(run(r, []string{"-u", "alice"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(Equal("scancel: error: error: commlib error: got select error\n"))
	})

	It("still fails when qdel -u fails and qstat cannot say whether jobs are left", func() {
		r := cluster(func(string, []string) fake.Response { return fake.Response{Exit: 1, Stderr: []byte("qmaster down")} },
			refusal)
		var errBuf bytes.Buffer
		Expect(run(r, []string{"-u", "alice"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("no job registered"))
	})

	It("does not ask qstat when qdel -u succeeded", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"-u", "alice"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls).To(HaveLen(1))
	})

	It("reports a qdel -u that could not be started", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: -1, Err: errors.New("exec: \"qdel\": executable file not found in $PATH")}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"--me"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(HavePrefix("scancel: error: running qdel: "))
	})
})

var _ = Describe("scancel with a filter, when the job changes between listing and cancel", func() {
	// The listing and the cancel are two steps. A job that finishes in between
	// is gone, not a failure.
	It("exits 0 when the listed job has finished by the time of the qdel", func() {
		r := cluster(queue, fake.Response{Exit: 1, Stdout: []byte(gone)})
		var errBuf bytes.Buffer
		Expect(run(r, []string{"-u", "alice", "4711"}, io.Discard, &errBuf)).To(Equal(0))
		Expect(errBuf.String()).To(BeEmpty())
		Expect(qdels(r)).To(Equal([][]string{{"4711"}}))
	})

	It("still fails when the listed job cannot be cancelled", func() {
		r := cluster(queue, fake.Response{Exit: 1,
			Stdout: []byte("alice - you do not have the necessary privileges to delete the job \"4711\"\n")})
		var errBuf bytes.Buffer
		Expect(run(r, []string{"-u", "alice", "4711"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("necessary privileges"))
	})
})

// queue lists, per GE user, alice's running job 4711, bob's pending job 4712,
// and alice's array 4720 with GE task 1 running and tasks 2-4 pending.
func queue(_ string, args []string) fake.Response {
	alice := `
    <job_list state="running">
      <JB_job_number>4711</JB_job_number><JB_name>train</JB_name><JB_owner>alice</JB_owner>
      <state>r</state><queue_name>all.q@node1</queue_name><slots>1</slots>
    </job_list>
    <job_list state="running">
      <JB_job_number>4720</JB_job_number><JB_name>sweep</JB_name><JB_owner>alice</JB_owner>
      <state>r</state><queue_name>gpu.q@node2</queue_name><slots>1</slots><tasks>1</tasks>
    </job_list>`
	alicePending := `
    <job_list state="pending">
      <JB_job_number>4720</JB_job_number><JB_name>sweep</JB_name><JB_owner>alice</JB_owner>
      <state>qw</state><slots>1</slots><tasks>2-4:1</tasks>
    </job_list>`
	bobPending := `
    <job_list state="pending">
      <JB_job_number>4712</JB_job_number><JB_name>train</JB_name><JB_owner>bob</JB_owner>
      <state>qw</state><slots>1</slots>
    </job_list>`
	running, pending := alice, alicePending
	if args[len(args)-1] == "*" {
		pending += bobPending
	}
	return fake.Response{Stdout: []byte("<?xml version='1.0'?><job_info><queue_info>" + running +
		"</queue_info><job_info>" + pending + "</job_info></job_info>")}
}

func qdels(r *fake.Runner) [][]string {
	var out [][]string
	for _, c := range r.Calls {
		if c.Name == "qdel" {
			out = append(out, c.Args)
		}
	}
	return out
}

var _ = Describe("scancel filters [REQ-SCL-002]", func() {
	It("intersects -u with the ids: never cancels another user's job or all of the user's jobs", func() {
		r := &fake.Runner{Responder: queue}
		Expect(run(r, []string{"-u", "alice", "4711", "4712"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(qdels(r)).To(Equal([][]string{{"4711"}}), "4712 is bob's; alice's other jobs are not named")
	})

	It("keeps the atomic qdel -u for -u alone", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"-u", "alice"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls[0].Args).To(Equal([]string{"-u", "alice"}))
	})

	It("cancels only the pending elements of a half-running array", func() {
		r := &fake.Runner{Responder: queue}
		Expect(run(r, []string{"-t", "PENDING", "4720"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(qdels(r)).To(Equal([][]string{{"4720", "-t", "2"}, {"4720", "-t", "3"}, {"4720", "-t", "4"}}))
	})

	It("filters by name and by partition", func() {
		r := &fake.Runner{Responder: queue}
		Expect(run(r, []string{"-n", "train"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(qdels(r)).To(ConsistOf([]string{"4711"}, []string{"4712"}))

		cfg := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(cfg, []byte("partitions:\n  gpu: {queue: gpu.q, pe: make}\n"), 0o644)).To(Succeed())
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfg)
		r = &fake.Runner{Responder: queue}
		Expect(run(r, []string{"-p", "gpu"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(qdels(r)).To(Equal([][]string{{"4720", "-t", "1"}}), "the running element's queue is gpu.q; pending ones have no queue yet")
	})

	It("rejects an unknown state", func() {
		var errBuf bytes.Buffer
		Expect(run(&fake.Runner{}, []string{"-t", "LOST", "4711"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("invalid job state"))
	})
})

var _ = Describe("scancel signals [REQ-SCL-003]", func() {
	DescribeTable("maps the signals Grid Engine can deliver",
		func(args []string, client string, want []string) {
			r := &fake.Runner{}
			Expect(run(r, args, io.Discard, io.Discard)).To(Equal(0))
			Expect(r.Calls).To(HaveLen(1))
			Expect(r.Calls[0].Name).To(Equal(client))
			Expect(r.Calls[0].Args).To(Equal(want))
		},
		Entry("TERM reschedules (submitit's preempt)", []string{"--signal", "TERM", "4711"}, "qmod", []string{"-rj", "4711"}),
		Entry("attached -sUSR2", []string{"-sUSR2", "4711"}, "qmod", []string{"-rj", "4711"}),
		Entry("SIGUSR1", []string{"-s", "SIGUSR1", "4711"}, "qmod", []string{"-rj", "4711"}),
		Entry("STOP suspends", []string{"-s", "STOP", "4711"}, "qmod", []string{"-sj", "4711"}),
		Entry("CONT resumes an element", []string{"-s", "CONT", "4711_0"}, "qmod", []string{"-usj", "4711.1"}),
		Entry("KILL cancels", []string{"-s", "9", "4711"}, "qdel", []string{"4711"}),
	)

	It("refuses a signal it cannot deliver instead of rescheduling", func() {
		r := &fake.Runner{}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"-s", "HUP", "4711"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(r.Calls).To(BeEmpty())
		Expect(errBuf.String()).To(ContainSubstring("no Grid Engine equivalent"))
	})
})
