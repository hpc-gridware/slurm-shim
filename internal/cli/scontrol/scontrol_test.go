package scontrol

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

func TestScontrol(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Scontrol Suite")
}

var _ = Describe("per-job state resolution [REQ-FAB-010]", func() {
	// scontrol used to read layout.json from a shared /tmp/slurm_shim when
	// TMPDIR was unset, a path any local user can plant a layout in. It must now
	// report "not inside a job" and fall through to the qstat lookup instead.
	It("does not read a layout from a shared /tmp path when TMPDIR is unset", func() {
		GinkgoT().Setenv("TMPDIR", "")
		_, err := loadLayout()
		Expect(err).To(MatchError(os.ErrNotExist))
	})

	It("reports no-job rather than reading /tmp when asked for its own job", func() {
		GinkgoT().Setenv("TMPDIR", "")
		var errBuf bytes.Buffer
		code := run(&fake.Runner{}, []string{"show", "job"}, io.Discard, &errBuf)
		Expect(code).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("not inside a job"))
	})
})

var _ = Describe("scontrol show hostnames [REQ-SCT-001]", func() {
	It("expands a nodelist to one hostname per line (AC4)", func() {
		var out bytes.Buffer
		code := run(&fake.Runner{}, []string{"show", "hostnames", "node[001-003,007]"}, &out, io.Discard)
		Expect(code).To(Equal(0))
		Expect(out.String()).To(Equal("node001\nnode002\nnode003\nnode007\n"))
	})

	It("accepts the singular 'show hostname' alias", func() {
		var out bytes.Buffer
		code := run(&fake.Runner{}, []string{"show", "hostname", "n[1-2]"}, &out, io.Discard)
		Expect(code).To(Equal(0))
		Expect(out.String()).To(Equal("n1\nn2\n"))
	})

	It("reads $SLURM_JOB_NODELIST when no argument is given", func() {
		GinkgoT().Setenv("SLURM_JOB_NODELIST", "gpu[01-02]")
		var out bytes.Buffer
		Expect(run(&fake.Runner{}, []string{"show", "hostnames"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(Equal("gpu01\ngpu02\n"))
	})

	It("errors on a malformed hostlist", func() {
		var errBuf bytes.Buffer
		code := run(&fake.Runner{}, []string{"show", "hostnames", "node[1-"}, io.Discard, &errBuf)
		Expect(code).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("Invalid hostlist"))
	})
})

var _ = Describe("scontrol requeue [REQ-SCT-002]", func() {
	It("maps a compound array id to a task-scoped qmod -rj", func() {
		r := &fake.Runner{}
		code := run(r, []string{"requeue", "4711_2"}, io.Discard, io.Discard)
		Expect(code).To(Equal(0))
		Expect(r.Calls).To(HaveLen(1))
		Expect(r.Calls[0].Name).To(Equal("qmod"))
		Expect(r.Calls[0].Args).To(Equal([]string{"-rj", "4711.2"}))
	})

	It("maps a bare job id directly", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"requeue", "4711"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls[0].Args).To(Equal([]string{"-rj", "4711"}))
	})

	It("surfaces a clean error when qmod refuses (rerun disabled)", func() {
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("job 4711 is not rerunnable")}
		}}
		var errBuf bytes.Buffer
		code := run(r, []string{"requeue", "4711"}, io.Discard, &errBuf)
		Expect(code).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("not rerunnable"))
	})
})

var _ = Describe("scontrol show job [REQ-SCT-003]", func() {
	It("prints a minimal record with the layout node list", func() {
		tmp := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		lay := &layout.Layout{
			SchemaVersion: layout.SchemaVersion,
			Job:           layout.Job{JobID: 4711, Name: "train", User: "alice", UID: 1000, Partition: "gpu", SubmitDir: "/home/alice"},
			Nodes:         []layout.Node{{Host: "node001"}, {Host: "node002"}},
			Tasks:         layout.Tasks{NTasks: 16},
		}
		Expect(layout.Write(filepath.Join(tmp, layout.StateDir), lay)).To(Succeed())

		var out bytes.Buffer
		Expect(run(&fake.Runner{}, []string{"show", "job", "4711"}, &out, io.Discard)).To(Equal(0))
		s := out.String()
		Expect(s).To(ContainSubstring("JobId=4711"))
		Expect(s).To(ContainSubstring("NodeList=node[001-002]"))
		Expect(s).To(ContainSubstring("NumTasks=16"))
		Expect(s).To(ContainSubstring("UserId=alice(1000)"))
	})

	It("looks the job up in GE when there is no local layout", func() {
		GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir()) // no fabricated layout here
		r := &fake.Runner{Responder: func(_ string, _ []string) fake.Response {
			return fake.Response{Stdout: []byte(qstatRunning)}
		}}
		var out bytes.Buffer
		Expect(run(r, []string{"show", "job", "14"}, &out, io.Discard)).To(Equal(0))
		s := out.String()
		Expect(s).To(ContainSubstring("JobId=14"))
		Expect(s).To(ContainSubstring("JobName=wrap.sh"))
		Expect(s).To(ContainSubstring("JobState=RUNNING"))
		Expect(s).To(ContainSubstring("NodeList=ocs-worker1"))
		Expect(s).To(ContainSubstring("Partition=all.q"))
		Expect(s).To(ContainSubstring("UserId=gridware"))
		// -u * is what makes another user's job visible; guard the argv.
		Expect(r.Calls[0].Name).To(Equal("qstat"))
		Expect(r.Calls[0].Args).To(Equal([]string{"-xml", "-u", "*"}))
	})

	It("uses GE (not the local layout) when the requested id differs from the in-job layout", func() {
		tmp := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		lay := &layout.Layout{SchemaVersion: layout.SchemaVersion, Job: layout.Job{JobID: 4711, Name: "mine"}, Nodes: []layout.Node{{Host: "node001"}}, Tasks: layout.Tasks{NTasks: 1}}
		Expect(layout.Write(filepath.Join(tmp, layout.StateDir), lay)).To(Succeed())
		r := &fake.Runner{Responder: func(_ string, _ []string) fake.Response {
			return fake.Response{Stdout: []byte(qstatRunning)} // job 14
		}}
		var out bytes.Buffer
		Expect(run(r, []string{"show", "job", "14"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("JobId=14")) // GE record, not the layout's 4711
		Expect(out.String()).To(ContainSubstring("NodeList=ocs-worker1"))
		Expect(r.Calls).NotTo(BeEmpty()) // GE was queried
	})

	It("uses the layout (not GE) when the requested id matches the in-job layout", func() {
		tmp := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		lay := &layout.Layout{SchemaVersion: layout.SchemaVersion, Job: layout.Job{JobID: 4711, Name: "mine"}, Nodes: []layout.Node{{Host: "node001"}}, Tasks: layout.Tasks{NTasks: 1}}
		Expect(layout.Write(filepath.Join(tmp, layout.StateDir), lay)).To(Succeed())
		r := &fake.Runner{}
		var out bytes.Buffer
		Expect(run(r, []string{"show", "job", "4711"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("JobId=4711"))
		Expect(r.Calls).To(BeEmpty()) // layout path: qstat never called
	})

	It("shows the in-job layout for a bare 'show job' (no id)", func() {
		tmp := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		lay := &layout.Layout{SchemaVersion: layout.SchemaVersion, Job: layout.Job{JobID: 77, Name: "mine"}, Nodes: []layout.Node{{Host: "node001"}}, Tasks: layout.Tasks{NTasks: 1}}
		Expect(layout.Write(filepath.Join(tmp, layout.StateDir), lay)).To(Succeed())
		r := &fake.Runner{}
		var out bytes.Buffer
		Expect(run(r, []string{"show", "job"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("JobId=77"))
		Expect(r.Calls).To(BeEmpty())
	})

	It("resolves an array-task id to that specific task, not the first base match", func() {
		GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir())
		r := &fake.Runner{Responder: func(_ string, _ []string) fake.Response {
			return fake.Response{Stdout: []byte(qstatArray)}
		}}
		// Task 5 is RUNNING (first row); task 2 is PENDING. Asking for 4711_2 must
		// report task 2's state/node, not task 5's.
		var out bytes.Buffer
		Expect(run(r, []string{"show", "job", "4711_2"}, &out, io.Discard)).To(Equal(0))
		s := out.String()
		Expect(s).To(ContainSubstring("JobId=4711_2"))
		Expect(s).To(ContainSubstring("JobState=PENDING"))
		Expect(s).To(ContainSubstring("NodeList=(null)"))
		Expect(s).NotTo(ContainSubstring("node5"))
	})

	It("errors cleanly on qstat spawn failure, non-zero exit, and unparseable output", func() {
		GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir())
		cases := []struct {
			resp fake.Response
			want string
		}{
			{fake.Response{Err: errors.New("boom")}, "running qstat"},
			{fake.Response{Exit: 1, Stderr: []byte("qmaster unreachable")}, "qmaster unreachable"},
			{fake.Response{Exit: 1}, "qstat failed"}, // non-zero exit, empty stderr
			{fake.Response{Stdout: []byte("<not-xml")}, "parsing qstat output"},
		}
		for _, c := range cases {
			r := &fake.Runner{Responder: func(_ string, _ []string) fake.Response { return c.resp }}
			var errBuf bytes.Buffer
			Expect(run(r, []string{"show", "job", "1"}, io.Discard, &errBuf)).To(Equal(1))
			Expect(errBuf.String()).To(ContainSubstring(c.want))
		}
	})

	It("errors with 'Invalid job id' when the job is unknown to GE", func() {
		GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir())
		r := &fake.Runner{Responder: func(_ string, _ []string) fake.Response {
			return fake.Response{Stdout: []byte(qstatRunning)}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"show", "job", "999"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("Invalid job id"))
	})

	It("errors when no job id is given outside a job", func() {
		GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir())
		var errBuf bytes.Buffer
		Expect(run(&fake.Runner{}, []string{"show", "job"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("no job id specified"))
	})
})

const qstatRunning = `<?xml version='1.0'?>
<job_info>
  <queue_info>
    <job_list state="running">
      <JB_job_number>14</JB_job_number>
      <JB_name>wrap.sh</JB_name>
      <JB_owner>gridware</JB_owner>
      <state>r</state>
      <queue_name>all.q@ocs-worker1</queue_name>
      <slots>1</slots>
    </job_list>
  </queue_info>
</job_info>`

// Array job 4711: task 5 running on node5 (a queue_info row), task 2 pending (a
// job_info row). Two rows share JB_job_number 4711 with distinct <tasks>.
const qstatArray = `<?xml version='1.0'?>
<job_info>
  <queue_info>
    <job_list state="running">
      <JB_job_number>4711</JB_job_number>
      <JB_name>arr</JB_name>
      <JB_owner>alice</JB_owner>
      <state>r</state>
      <queue_name>all.q@node5</queue_name>
      <slots>1</slots>
      <tasks>5</tasks>
    </job_list>
  </queue_info>
  <job_info>
    <job_list state="pending">
      <JB_job_number>4711</JB_job_number>
      <JB_name>arr</JB_name>
      <JB_owner>alice</JB_owner>
      <state>qw</state>
      <slots>1</slots>
      <tasks>2</tasks>
    </job_list>
  </job_info>
</job_info>`

var _ = Describe("scontrol errors", func() {
	It("rejects an unknown subcommand", func() {
		var errBuf bytes.Buffer
		code := run(&fake.Runner{}, []string{"reconfigure"}, io.Discard, &errBuf)
		Expect(code).To(Equal(1))
		Expect(errBuf.String()).To(HavePrefix("scontrol: error:"))
	})
})

var _ = Describe("scontrol dry run [SLURM_SHIM_DRY_RUN]", func() {
	It("reports the requeue and reschedules nothing", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		inner := &fake.Runner{}
		var errBuf bytes.Buffer
		code := run(dryrun.Wrap(inner, &errBuf, "scontrol"),
			[]string{"requeue", "4711_2"}, io.Discard, &errBuf)

		Expect(code).To(Equal(0))
		Expect(inner.Calls).To(BeEmpty(), "a dry run must not reach the cluster")
		Expect(errBuf.String()).To(ContainSubstring("would run: qmod -rj 4711.2"))
	})

	// The read-only client behind `show job` must still run, or the wrapper would
	// break a subcommand that changes nothing.
	It("still answers show job, whose qstat is read-only", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		inner := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			return fake.Response{Stdout: []byte(qstatShowJob)}
		}}
		var out, errBuf bytes.Buffer
		code := run(dryrun.Wrap(inner, &errBuf, "scontrol"),
			[]string{"show", "job", "4711"}, &out, &errBuf)

		Expect(code).To(Equal(0))
		Expect(inner.Calls).To(HaveLen(1))
		Expect(inner.Calls[0].Name).To(Equal("qstat"))
		Expect(out.String()).To(ContainSubstring("JobId=4711"))
	})
})

// qstatShowJob is a single running job 4711, for the dry-run show-job spec.
const qstatShowJob = `<?xml version='1.0'?>
<job_info>
  <queue_info>
    <job_list state="running">
      <JB_job_number>4711</JB_job_number>
      <JB_name>train</JB_name>
      <JB_owner>alice</JB_owner>
      <state>r</state>
      <queue_name>all.q@node1</queue_name>
      <slots>4</slots>
    </job_list>
  </queue_info>
</job_info>`
