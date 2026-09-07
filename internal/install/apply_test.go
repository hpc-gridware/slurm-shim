package install_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/install"
)

var _ = Describe("Apply", func() {
	ctx := context.Background()

	It("performs exactly the mutating changes, then a second plan is empty", func() {
		f := bare()
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix})
		r := install.Apply(ctx, f, p)

		Expect(r.Failed()).To(BeEmpty())
		Expect(r.Applied()).To(Equal(3), "add PE, add to pe_list, set starter")
		Expect(f.calls).To(ConsistOf(
			"AddPE slurm-shim",
			"AddQueueAttr all.q pe_list+=slurm-shim",
			"SetQueueAttr all.q starter_method="+prefix+"/bin/slurm-shim-starter",
		))

		again, _ := install.Discover(ctx, f)
		Expect(install.MakePlan(again, install.Options{Prefix: prefix}).Mutating()).To(BeFalse(),
			"apply twice must change nothing")
	})

	It("adds to pe_list rather than rewriting it (site PEs survive)", func() {
		f := bare()
		facts, _ := install.Discover(ctx, f)
		install.Apply(ctx, f, install.MakePlan(facts, install.Options{Prefix: prefix}))
		Expect(f.queues["all.q"].PEList).To(Equal([]string{"make", "slurm-shim"}))
	})

	It("skips refusals and unchanged rows without calling the cluster", func() {
		f := bare()
		q := f.queues["all.q"]
		q.StarterMethod = "/site/starter.sh"
		f.queues["all.q"] = q
		facts, _ := install.Discover(ctx, f)
		install.Apply(ctx, f, install.MakePlan(facts, install.Options{Prefix: prefix}))
		Expect(f.queues["all.q"].StarterMethod).To(Equal("/site/starter.sh"), "refused change must not be applied")
		Expect(f.calls).NotTo(ContainElement(ContainSubstring("starter_method")))
	})

	It("keeps going after one failure and reports it (changes are independent)", func() {
		f := bare()
		f.failOn = "AddPE"
		facts, _ := install.Discover(ctx, f)
		r := install.Apply(ctx, f, install.MakePlan(facts, install.Options{Prefix: prefix}))

		Expect(r.Failed()).To(HaveLen(1))
		Expect(r.Failed()[0].Err.Error()).To(ContainSubstring("add-pe slurm-shim"))
		Expect(f.queues["all.q"].StarterMethod).To(Equal(prefix+"/bin/slurm-shim-starter"),
			"the queue change must still have been applied")
	})
})
