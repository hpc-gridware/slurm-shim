package squeue

import (
	"bytes"
	"io"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func TestSqueue(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Squeue Suite")
}

const qstatXML = `<?xml version='1.0'?>
<job_info>
  <queue_info>
    <job_list state="running">
      <JB_job_number>4711</JB_job_number>
      <JB_name>train</JB_name>
      <JB_owner>alice</JB_owner>
      <state>r</state>
      <queue_name>gpu.q@node001</queue_name>
      <slots>8</slots>
    </job_list>
  </queue_info>
  <job_info>
    <job_list state="pending">
      <JB_job_number>4712</JB_job_number>
      <JB_name>test</JB_name>
      <JB_owner>bob</JB_owner>
      <state>qw</state>
      <slots>4</slots>
    </job_list>
    <job_list state="pending">
      <JB_job_number>4713</JB_job_number>
      <JB_name>arr</JB_name>
      <JB_owner>alice</JB_owner>
      <state>qw</state>
      <slots>1</slots>
      <tasks>2</tasks>
    </job_list>
  </job_info>
</job_info>`

func testCfg() *config.Config {
	c := config.Default()
	c.PartitionAliases = map[string]string{"gpu.q": "gpu", "all.q": "batch"}
	return c
}

func fakeQstat() *fake.Runner {
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		return fake.Response{Stdout: []byte(qstatXML)}
	}}
}

var _ = Describe("squeue [REQ-SQU-002]", func() {
	It("renders the default 8-column header and mapped states", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), nil, &out, io.Discard)).To(Equal(0))
		s := out.String()
		Expect(s).To(HavePrefix("             JOBID PARTITION"))
		Expect(s).To(ContainSubstring("NODELIST(REASON)"))
		// Running job maps to R with its partition alias.
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		Expect(lines).To(HaveLen(4)) // header + 3 jobs
	})

	It("maps GE states to SLURM compact states [REQ-SQU-001]", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-o", "%i %t"}, &out, io.Discard)).To(Equal(0))
		rows := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		Expect(rows).To(ContainElement("4711 R"))
		Expect(rows).To(ContainElement("4712 PD"))
		Expect(rows).To(ContainElement("4713_2 PD")) // array task rendered 4713_2
	})

	It("maps a running job's queue to its partition alias [REQ-SQU-002]", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-j", "4711", "-o", "%i %P %R"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("4711 gpu node001"))
	})

	It("filters to a single job with -j", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-j", "4711", "-o", "%i"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("4711"))
	})

	It("filters to a single array task", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-j", "4713_2", "-o", "%i"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("4713_2"))
	})

	It("shows only the header for a completed (absent) job [REQ-SQU-003]", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-j", "9999", "-o", "%i %t"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimRight(out.String(), "\n")).To(Equal("JOBID ST"))
	})

	It("suppresses the header with -h", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-o", "%i"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).NotTo(ContainSubstring("JOBID"))
	})

	It("surfaces a qstat failure", func() {
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qstat: cannot connect")}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, testCfg(), nil, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("cannot connect"))
	})
})
