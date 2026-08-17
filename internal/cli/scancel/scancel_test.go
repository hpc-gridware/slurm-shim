package scancel

import (
	"bytes"
	"io"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

	It("maps an array task to qdel -t", func() {
		r := &fake.Runner{}
		Expect(run(r, []string{"4711_2"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls[0].Args).To(Equal([]string{"-t", "2", "4711"}))
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

	It("warns that --signal is unsupported but still cancels", func() {
		r := &fake.Runner{}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"--signal=USR1", "4711"}, io.Discard, &errBuf)).To(Equal(0))
		Expect(errBuf.String()).To(ContainSubstring("--signal is not supported"))
		Expect(r.Calls[0].Args).To(Equal([]string{"4711"}))
	})

	It("surfaces a qdel failure exit code", func() {
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("denied: job 4711 does not exist")}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, []string{"4711"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("does not exist"))
	})

	It("errors when no job id is given", func() {
		Expect(run(&fake.Runner{}, nil, io.Discard, io.Discard)).To(Equal(1))
	})
})
