package srun

import (
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These in-process specs register into the same Ginkgo suite driven by
// srun_suite_test.go's RunSpecs (internal and external test packages share one
// test binary).

var _ = Describe("srun flag parsing", func() {
	parse := func(args ...string) (*options, error) {
		return parseFlags(args, false, io.Discard)
	}

	It("passes everything after the first non-flag token through verbatim [REQ-RUN-006]", func() {
		opt, err := parse("-n", "4", "sh", "-c", "echo -n hi")
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.req.NTasks).To(Equal(4))
		Expect(opt.command).To(Equal([]string{"sh", "-c", "echo -n hi"}))
	})

	It("recognizes the core flag surface [REQ-RUN-001]", func() {
		opt, err := parse("-N", "2", "--ntasks-per-node", "3", "-c", "4", "-l", "hostname")
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.req.Nodes).To(Equal(2))
		Expect(opt.req.TasksPerNode).To(Equal(3))
		Expect(opt.req.CPUsPerTask).To(Equal(4))
		Expect(opt.label).To(BeTrue())
	})

	It("warns on an unknown flag and continues [REQ-RUN-005]", func() {
		opt, err := parse("--frobnicate", "-n", "1", "hostname")
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.warnings).To(ContainElement(ContainSubstring("--frobnicate")))
		Expect(opt.command).To(Equal([]string{"hostname"}))
	})

	It("fails an unknown flag under strict_flags [REQ-RUN-005]", func() {
		_, err := parseFlags([]string{"--frobnicate", "hostname"}, true, io.Discard)
		Expect(err).To(HaveOccurred())
	})

	It("rejects an unsupported --mpi and accepts none [REQ-RUN-004]", func() {
		_, err := parse("--mpi", "pmix", "hostname")
		Expect(err).To(HaveOccurred())
		opt, err := parse("--mpi", "none", "hostname")
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.command).To(Equal([]string{"hostname"}))
	})

	It("treats -K as kill-on with an optional value", func() {
		opt, _ := parse("-K", "hostname")
		Expect(opt.killFlag).To(Equal("1"))
		opt, _ = parse("--kill-on-bad-exit=0", "hostname")
		Expect(opt.killFlag).To(Equal("0"))
	})

	It("expands a --nodelist range [REQ-RUN-002]", func() {
		opt, err := parse("-w", "node[001-003]", "hostname")
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.req.Nodelist).To(Equal([]string{"node001", "node002", "node003"}))
	})
})
