package scontrol

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

func TestScontrol(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Scontrol Suite")
}

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
})

var _ = Describe("scontrol errors", func() {
	It("rejects an unknown subcommand", func() {
		var errBuf bytes.Buffer
		code := run(&fake.Runner{}, []string{"reconfigure"}, io.Discard, &errBuf)
		Expect(code).To(Equal(1))
		Expect(errBuf.String()).To(HavePrefix("scontrol: error:"))
	})
})
