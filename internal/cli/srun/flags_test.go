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

	It("parses submitit's generated srun line with the command intact [submitit Phase 5]", func() {
		opt, err := parse("--unbuffered", "--output", "o_%A_%a.log", "--error", "e.log",
			"--cpu-bind", "none", "python", "-m", "submitit.core._submit", "/tmp/folder")
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.warnings).To(BeEmpty())
		Expect(opt.output).To(Equal("o_%A_%a.log"))
		Expect(opt.errorPat).To(Equal("e.log"))
		Expect(opt.command).To(Equal([]string{"python", "-m", "submitit.core._submit", "/tmp/folder"}))
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

	DescribeTable("steps over SLURM options it does not translate, never the command [REQ-RUN-005]",
		func(args []string, wantWarn string) {
			opt, err := parse(args...)
			Expect(err).NotTo(HaveOccurred())
			Expect(opt.command).To(Equal([]string{"python", "x.py"}))
			if wantWarn != "" {
				Expect(opt.warnings).To(ContainElement(ContainSubstring(wantWarn)))
			}
		},
		Entry("-u is --unbuffered, no value", []string{"-u", "python", "x.py"}, ""),
		Entry("-I takes its value only attached", []string{"-I", "python", "x.py"}, ""),
		Entry("-I60", []string{"-I60", "python", "x.py"}, ""),
		Entry("-x takes a value", []string{"-x", "node3", "python", "x.py"}, ""),
		Entry("--exclude=", []string{"--exclude=node3", "python", "x.py"}, ""),
		Entry("-L takes a value", []string{"-L", "lic:2", "python", "x.py"}, ""),
		Entry("-C takes a value", []string{"-C", "avx512", "python", "x.py"}, ""),
		Entry("-B takes a value", []string{"-B", "2:4", "python", "x.py"}, "--extra-node-info"),
		Entry("-m takes a value", []string{"-m", "block", "python", "x.py"}, "--distribution"),
		Entry("--multi-prog takes no value", []string{"--multi-prog", "python", "x.py"}, "--multi-prog"),
		Entry("--exclusive=user", []string{"--exclusive=user", "python", "x.py"}, ""),
		Entry("bare --exclusive", []string{"--exclusive", "python", "x.py"}, ""),
		Entry("--x11=all", []string{"--x11=all", "python", "x.py"}, "--x11"),
	)

	It("translates -G as --gpus instead of dropping its value", func() {
		opt, err := parse("--pty", "-G", "2", "python", "x.py")
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.command).To(Equal([]string{"python", "x.py"}))
		Expect(opt.haveGPUs).To(BeTrue())
		Expect(opt.gpus).To(Equal(2))
	})

	It("fails a SLURM option it does not translate under strict_flags [REQ-RUN-005]", func() {
		_, err := parseFlags([]string{"-m", "block", "hostname"}, true, io.Discard)
		Expect(err).To(MatchError(ContainSubstring("--distribution")))
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

var _ = Describe("srun allocation memory and value checks [todos 207, 208]", func() {
	parse := func(args ...string) (*options, error) { return parseFlags(args, false, io.Discard) }

	DescribeTable("takes the last of --mem, --mem-per-cpu and --mem-per-gpu",
		func(wantMem, wantKind string, args ...string) {
			opt, err := parse(append(args, "hostname")...)
			Expect(err).NotTo(HaveOccurred())
			Expect(opt.mem).To(Equal(wantMem))
			Expect(opt.memKind).To(Equal(wantKind))
		},
		// SALLOC_MEM_PER_CPU arrives prepended, so a --mem on the command line wins.
		Entry("--mem after an environment --mem-per-cpu", "8G", "", "--mem-per-cpu=2G", "--mem=8G"),
		Entry("--mem-per-cpu after --mem", "2G", "mem-per-cpu", "--mem=8G", "--mem-per-cpu=2G"),
		Entry("--mem-per-cpu after --mem-per-gpu", "1G", "mem-per-cpu", "--mem-per-gpu=4G", "--mem-per-cpu", "1G"),
		Entry("--mem-per-gpu after --mem-per-cpu", "4G", "mem-per-gpu", "--mem-per-cpu=1G", "--mem-per-gpu=4G"),
	)

	DescribeTable("refuses values that would change the request they land in",
		func(msg string, args ...string) {
			_, err := parse(append(args, "hostname")...)
			Expect(err).To(MatchError(ContainSubstring(msg)))
		},
		Entry("--mem with an extra -l entry", "invalid memory value", "--mem=1G,slots=9"),
		Entry("--tmp with an extra -l entry", "invalid memory value", "--tmp=1G,h_rt=99999"),
		Entry("--mem-per-cpu that is not a size", "invalid memory value", "--mem-per-cpu=lots"),
		Entry("an -x host with an operator", "is not a host name", "-x", "a,!b"),
		Entry("a -w host with a request", "is not a host name", "-w", "n1,h_vmem=1"),
	)
})
