package scancel

import (
	"bytes"
	"io"
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
