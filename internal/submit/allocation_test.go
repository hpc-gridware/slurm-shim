package submit_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

// qsub -help text with and without the -par usage line, so the probe's OCS-version
// gate can be exercised without a cluster.
func qsubHelp(withPar bool) *fake.Runner {
	body := "usage: qsub [options]\n  [-q wc_queue_list]\n"
	if withPar {
		body += "  [-par allocation_rule]                   set the parallel job allocation rule\n"
	}
	return &fake.Runner{Responder: func(string, []string) fake.Response {
		return fake.Response{Stdout: []byte(body)}
	}}
}

var _ = Describe("AllocationRuleProbe / AllocationRuleFor [shared -par gate]", func() {
	perTask := config.Partition{Queue: "all.q", PE: "make", Slots: "per-task"}
	cfg := &config.Config{AllocationRuleOverride: config.OverrideAuto}

	It("reports supported when qsub advertises -par", func() {
		ok, warn := submit.AllocationRuleProbe(context.Background(), qsubHelp(true))
		Expect(ok).To(BeTrue())
		Expect(warn).To(BeEmpty())
	})

	It("warns that the cluster is too old when -par is absent", func() {
		ok, warn := submit.AllocationRuleProbe(context.Background(), qsubHelp(false))
		Expect(ok).To(BeFalse())
		Expect(warn).To(ContainSubstring("no -par"))
		Expect(warn).To(ContainSubstring("9.1.5"))
	})

	It("distinguishes a probe failure from an old cluster", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Err: context.DeadlineExceeded}
		}}
		ok, warn := submit.AllocationRuleProbe(context.Background(), r)
		Expect(ok).To(BeFalse())
		Expect(warn).To(ContainSubstring("could not probe"))
	})

	It("emits a rule for -N 2 on a supported cluster", func() {
		req := submit.Request{Nodes: 2}
		rule, warns := submit.AllocationRuleFor(context.Background(), qsubHelp(true), cfg, req, perTask, 2)
		Expect(rule.Value).To(Equal("1"), "2 slots over 2 nodes = 1 per node")
		Expect(warns).To(BeEmpty())
	})

	It("declines to emit (with a warning) when the cluster has no -par", func() {
		req := submit.Request{Nodes: 2}
		rule, warns := submit.AllocationRuleFor(context.Background(), qsubHelp(false), cfg, req, perTask, 2)
		Expect(rule.Emit()).To(BeFalse())
		Expect(warns).To(ContainElement(ContainSubstring("no -par")))
	})

	It("emits nothing and probes nothing when no layout was requested", func() {
		r := qsubHelp(false) // would warn IF probed
		rule, warns := submit.AllocationRuleFor(context.Background(), r, cfg, submit.Request{}, perTask, 4)
		Expect(rule.Emit()).To(BeFalse())
		Expect(warns).To(BeEmpty(), "no layout stated -> no probe, no warning")
	})

	It("honours allocation_rule_override: never (no probe, no rule)", func() {
		never := &config.Config{AllocationRuleOverride: config.OverrideNever}
		rule, warns := submit.AllocationRuleFor(context.Background(), qsubHelp(true), never, submit.Request{Nodes: 2}, perTask, 2)
		Expect(rule.Emit()).To(BeFalse())
		Expect(warns).To(BeEmpty())
	})
})
