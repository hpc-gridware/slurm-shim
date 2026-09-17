package launch

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

var _ = Describe("per-slot queue memory limits [SI-18]", func() {
	queue := func(resp fake.Response) *fake.Runner {
		return &fake.Runner{Responder: func(name string, args []string) fake.Response {
			Expect(name).To(Equal("qconf"))
			Expect(args).To(Equal([]string{"-sq", "all.q"}))
			return resp
		}}
	}
	ctx := context.Background()

	It("names only the limits that are not INFINITY, sorted", func() {
		r := queue(fake.Response{Stdout: []byte("qname all.q\nh_vmem INFINITY\ns_rss 2G\nh_data 8G\nh_rt 1:0:0\n")})
		limits, err := ReadQueueMemoryLimits(ctx, r, "all.q")
		Expect(err).NotTo(HaveOccurred())
		Expect(limits).To(Equal([]string{"h_data=8G", "s_rss=2G"}), "h_rt is not a memory limit")
	})

	It("reports no limits, and no error, for a queue at the GE defaults", func() {
		r := queue(fake.Response{Stdout: []byte("qname all.q\nh_vmem INFINITY\nh_stack INFINITY\n")})
		limits, err := ReadQueueMemoryLimits(ctx, r, "all.q")
		Expect(err).NotTo(HaveOccurred())
		Expect(limits).To(BeEmpty())
	})

	It("reads the cluster queue for a queue instance", func() {
		r := queue(fake.Response{Stdout: []byte("h_vmem 4G\n")})
		Expect(ReadQueueMemoryLimits(ctx, r, "all.q@node1")).To(Equal([]string{"h_vmem=4G"}))
	})

	It("keeps a read failure instead of passing it off as no limits", func() {
		// doctor turns "no limits" into a pass line; an unreadable queue must
		// not become one.
		_, err := ReadQueueMemoryLimits(ctx, queue(fake.Response{Exit: 1, Stderr: []byte("no such queue")}), "all.q")
		Expect(err).To(MatchError(ContainSubstring("no such queue")))

		_, err = ReadQueueMemoryLimits(ctx, queue(fake.Response{Err: errors.New("qconf not found")}), "all.q")
		Expect(err).To(MatchError(ContainSubstring("qconf not found")))

		_, err = ReadQueueMemoryLimits(ctx, queue(fake.Response{}), "")
		Expect(err).To(HaveOccurred())
	})

	It("recognises the per-slot memory limits by name", func() {
		for _, n := range []string{"h_vmem", "s_vmem", "h_rss", "s_rss", "h_data", "s_data", "h_stack", "s_stack", "H_VMEM"} {
			Expect(IsPerSlotMemoryLimit(n)).To(BeTrue(), n)
		}
		for _, n := range []string{"mem_free", "virtual_free", "h_rt", ""} {
			Expect(IsPerSlotMemoryLimit(n)).To(BeFalse(), n)
		}
	})

	It("stays quiet on a read failure for srun's preflight", func() {
		Expect(QueueMemoryLimits(ctx, queue(fake.Response{Exit: 1}), "all.q")).To(BeNil())
	})
})
