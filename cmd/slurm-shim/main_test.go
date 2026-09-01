package main

import (
	"bytes"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("arg0 dispatch", func() {
	var stdout, stderr *bytes.Buffer

	BeforeEach(func() {
		stdout = &bytes.Buffer{}
		stderr = &bytes.Buffer{}
	})

	It("treats symlink and subcommand forms identically [REQ-DLV-001]", func() {
		viaSymlink := run("srun", []string{"--version"}, stdout, stderr)

		out2 := &bytes.Buffer{}
		viaSubcommand := run("slurm-shim", []string{"srun", "--version"}, out2, &bytes.Buffer{})

		Expect(viaSymlink).To(Equal(viaSubcommand))
		Expect(stdout.String()).To(Equal(out2.String()))
	})

	DescribeTable("every shimmed command answers --version and -V [REQ-DLV-003]",
		func(arg0 string, flag string) {
			out := &bytes.Buffer{}
			code := run(arg0, []string{flag}, out, &bytes.Buffer{})
			Expect(code).To(Equal(0))
			Expect(out.String()).To(HavePrefix("slurm "))
			Expect(out.String()).To(ContainSubstring("slurm-shim "))
		},
		Entry("srun --version", "srun", "--version"),
		Entry("sbatch -V", "sbatch", "-V"),
		Entry("sinfo -V", "sinfo", "-V"),
		Entry("scontrol --version", "scontrol", "--version"),
		Entry("squeue --version", "squeue", "--version"),
		Entry("scancel -V", "scancel", "-V"),
	)

	It("prints version to stdout, never stderr [REQ-LOG-003]", func() {
		run("srun", []string{"--version"}, stdout, stderr)
		Expect(stdout.Len()).To(BeNumerically(">", 0))
		Expect(stderr.Len()).To(Equal(0))
	})

	It("errors with the command-name prefix for unimplemented commands [REQ-LOG-001]", func() {
		code := run("srun", []string{"hostname"}, stdout, stderr)
		Expect(code).To(Equal(1))
		Expect(stdout.Len()).To(Equal(0))
		Expect(stderr.String()).To(HavePrefix("srun: error:"))
	})

	It("rejects an unknown command name [REQ-DLV-001]", func() {
		code := run("notacommand", nil, stdout, stderr)
		Expect(code).To(Equal(1))
		Expect(stderr.String()).To(HavePrefix("slurm-shim: error: unknown command"))
	})

	It("rejects the bare binary with no subcommand", func() {
		code := run("slurm-shim", nil, stdout, stderr)
		Expect(code).To(Equal(1))
		Expect(stderr.String()).To(ContainSubstring("no command given"))
	})

	It("keeps all diagnostics off stdout [REQ-LOG-003]", func() {
		run("notacommand", nil, stdout, stderr)
		Expect(stdout.Len()).To(Equal(0))
		Expect(strings.TrimSpace(stderr.String())).NotTo(BeEmpty())
	})
})

// These drive the real Run() entry points, so they fail if dryrun.Wrap is ever
// removed from a mutating command's wiring -- the unit specs drive the unexported
// run() and would stay green.
var _ = Describe("dry-run wiring at the command entry points [SLURM_SHIM_DRY_RUN]", func() {
	var stdout, stderr *bytes.Buffer

	BeforeEach(func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		stdout = &bytes.Buffer{}
		stderr = &bytes.Buffer{}
	})

	It("intercepts scancel before it can reach qdel", func() {
		code := run("scancel", []string{"4711"}, stdout, stderr)

		Expect(code).To(Equal(0))
		Expect(stderr.String()).To(ContainSubstring("dry run"))
		Expect(stderr.String()).To(ContainSubstring("would run: qdel 4711"))
		Expect(stdout.String()).To(BeEmpty(), "real scancel writes nothing to stdout")
	})

	It("intercepts scontrol requeue before it can reach qmod", func() {
		code := run("scontrol", []string{"requeue", "4711_2"}, stdout, stderr)

		Expect(code).To(Equal(0))
		Expect(stderr.String()).To(ContainSubstring("would run: qmod -rj 4711.2"))
		Expect(stdout.String()).To(BeEmpty())
	})
})
