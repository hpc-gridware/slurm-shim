package doctor

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("daemon_forks_slaves verdict [SI-18, REQ-APX-003]", func() {
	unlimited := queueLimits{Queue: "all.q"}
	capped := queueLimits{Queue: "big.q", Limits: []string{"h_vmem=4G"}}

	It("PASSES the installer's FALSE when no queue offering the PE caps per-slot memory", func() {
		// The regression: doctor warned on FALSE every time, so a correctly
		// configured cluster could never get a clean report for this PE.
		warns, pass := forksFindings("make", false, "mem_free", []queueLimits{unlimited, {Queue: "gpu.q"}})
		Expect(warns).To(BeEmpty())
		Expect(pass).To(ContainSubstring("pe make daemon_forks_slaves FALSE"))
		Expect(pass).To(ContainSubstring("all.q, gpu.q"), "the pass names what it checked")
	})

	It("warns on FALSE only for the queue that caps memory, naming the limit", func() {
		warns, pass := forksFindings("make", false, "mem_free", []queueLimits{unlimited, capped})
		Expect(pass).To(BeEmpty())
		Expect(warns).To(HaveLen(1))
		Expect(warns[0]).To(ContainSubstring(`queue "big.q" sets h_vmem=4G`))
		Expect(warns[0]).To(ContainSubstring("SI-18"))
	})

	It("does not pass a FALSE PE whose queue could not be read", func() {
		warns, pass := forksFindings("make", false, "mem_free", []queueLimits{
			unlimited, {Queue: "odd.q", Err: errors.New("denied")},
		})
		Expect(pass).To(BeEmpty())
		Expect(warns).To(ConsistOf(And(ContainSubstring(`"odd.q"`), ContainSubstring("denied"))))
	})

	It("warns on TRUE exactly once, whatever the queues set", func() {
		// The regression: TRUE printed the same warning twice, once through
		// srun's preflight and once as the standing note.
		for _, queues := range [][]queueLimits{nil, {unlimited}, {unlimited, capped}} {
			warns, pass := forksFindings("make", true, "mem_free", queues)
			Expect(pass).To(BeEmpty())
			Expect(warns).To(HaveLen(1))
			Expect(warns[0]).To(ContainSubstring("concurrent srun steps will not run"))
		}
	})

	It("does not pass FALSE when memory_complex makes every --mem request a per-slot limit", func() {
		// The regression: with memory_complex h_vmem the queues carry no limit
		// of their own, but Grid Engine turns each job's -l h_vmem request into
		// the queue instance's enforced limit (sge_give_jobs.cc,
		// reduce_queue_limit). doctor printed PASS for exactly the SI-18 case.
		for _, mc := range []string{"h_vmem", "s_vmem", "h_rss", "h_data", "h_stack"} {
			warns, pass := forksFindings("make", false, mc, []queueLimits{unlimited})
			Expect(pass).To(BeEmpty(), mc)
			Expect(warns).To(HaveLen(1), mc)
			Expect(warns[0]).To(ContainSubstring("memory_complex " + mc))
			Expect(warns[0]).To(ContainSubstring("SI-18"))
		}
	})

	It("names both sources when a queue limit and memory_complex both apply", func() {
		warns, pass := forksFindings("make", false, "h_vmem", []queueLimits{capped})
		Expect(pass).To(BeEmpty())
		Expect(warns).To(HaveLen(2))
		Expect(warns[0]).To(ContainSubstring(`queue "big.q" sets h_vmem=4G`))
		Expect(warns[1]).To(ContainSubstring("memory_complex h_vmem"))
	})

	It("says nothing about FALSE when no readable queue offers the PE", func() {
		warns, pass := forksFindings("make", false, "mem_free", nil)
		Expect(warns).To(BeEmpty())
		Expect(pass).To(BeEmpty())
	})
})
