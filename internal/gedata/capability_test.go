package gedata_test

import (
	"context"
	"errors"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// usageRunner replays a captured `qsub -help` and records how often it was asked.
func usageRunner(usage []byte, exit int, err error) *fake.Runner {
	return &fake.Runner{Responder: func(name string, _ []string) fake.Response {
		if name != "qsub" {
			return fake.Response{}
		}
		return fake.Response{Stdout: usage, Exit: exit, Err: err}
	}}
}

func fixture(name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	Expect(err).NotTo(HaveOccurred())
	return b
}

var _ = Describe("Capabilities.AllocationRuleOverride", func() {
	ctx := context.Background()

	It("detects -par in a real OCS 9.1.5 qsub usage", func() {
		r := usageRunner(fixture("qsub-help-9.1.5.txt"), 0, nil)
		ok, err := gedata.NewCapabilities(r).AllocationRuleOverride(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(r.Calls).To(HaveLen(1))
		Expect(r.Calls[0].Name).To(Equal("qsub"))
		Expect(r.Calls[0].Args).To(Equal([]string{"-help"}))
	})

	It("reports no support when the usage has no -par entry", func() {
		ok, err := gedata.NewCapabilities(
			usageRunner(fixture("qsub-help-9.0.10-synthetic.txt"), 0, nil)).AllocationRuleOverride(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	// The distinction that keeps a missing GE client from being misreported as an
	// old cluster: err is set, and support stays false.
	It("returns the probe error rather than 'unsupported' when qsub cannot run", func() {
		boom := errors.New("exec: \"qsub\": executable file not found in $PATH")
		ok, err := gedata.NewCapabilities(usageRunner(nil, 0, boom)).AllocationRuleOverride(ctx)
		Expect(err).To(MatchError(boom))
		Expect(ok).To(BeFalse())
	})

	It("reports no support for empty usage output", func() {
		ok, err := gedata.NewCapabilities(usageRunner(nil, 0, nil)).AllocationRuleOverride(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	// Prose that merely names the option must not read as support -- the 9.0.10
	// fixture's own header line mentions it in exactly that way.
	It("ignores a bare -par mention outside the bracketed usage form", func() {
		ok, err := gedata.NewCapabilities(usageRunner(
			[]byte("OCS 9.0.10\nthis build does not support -par at all\n"), 0, nil)).AllocationRuleOverride(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("probes once per instance and reuses the answer", func() {
		r := usageRunner(fixture("qsub-help-9.1.5.txt"), 0, nil)
		caps := gedata.NewCapabilities(r)
		for i := 0; i < 3; i++ {
			ok, err := caps.AllocationRuleOverride(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		}
		Expect(r.Calls).To(HaveLen(1))
	})

	// Each instance probes independently: the cache must not leak across specs.
	It("does not share its answer with another instance", func() {
		r := usageRunner(fixture("qsub-help-9.0.10-synthetic.txt"), 0, nil)
		ok, err := gedata.NewCapabilities(r).AllocationRuleOverride(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		ok, err = gedata.NewCapabilities(
			usageRunner(fixture("qsub-help-9.1.5.txt"), 0, nil)).AllocationRuleOverride(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
	})
})
