package srun

import (
	"bytes"
	"io"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// The interactive path (srun --pty outside an allocation -> qrsh). These specs
// cover flag parsing (including the regression where an undefined flag ate the
// command), the translation into a qrsh line, and the dry run.
var _ = Describe("srun --pty flag parsing", func() {
	It("defines --pty so it does not swallow the command", func() {
		// Before --pty was a defined bool, `srun --pty bash` lost `bash` to pflag
		// and died with "no command given". It must now parse as pty + command.
		opt, err := parseFlags([]string{"--pty", "bash"}, false, io.Discard)
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.pty).To(BeTrue())
		Expect(opt.command).To(Equal([]string{"bash"}))
	})

	It("parses the interactive resource flags without eating the command", func() {
		opt, err := parseFlags(strings.Fields("--pty -p gpu -c 2 --mem 2G --time 0:30:00 -A acct --gres gpu:1 bash"), false, io.Discard)
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.command).To(Equal([]string{"bash"}))
		Expect(opt.partition).To(Equal("gpu"))
		Expect(opt.req.CPUsPerTask).To(Equal(2))
		Expect(opt.mem).To(Equal("2G"))
		Expect(opt.haveTime).To(BeTrue())
		Expect(opt.timeSec).To(Equal(1800))
		Expect(opt.account).To(Equal("acct"))
		Expect(opt.haveGPUs).To(BeTrue())
		Expect(opt.gpus).To(Equal(1))
	})

	It("warns and ignores --qos (no Grid Engine analogue)", func() {
		opt, err := parseFlags([]string{"--pty", "--qos", "debug", "bash"}, false, io.Discard)
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.command).To(Equal([]string{"bash"}))
		Expect(strings.Join(opt.warnings, " ")).To(ContainSubstring("--qos"))
	})
})

var _ = Describe("srun --pty interactive translation", func() {
	cfg := &config.Config{
		DefaultPartition: "batch",
		Partitions: map[string]config.Partition{
			"batch": {Queue: "all.q", PE: "make", Slots: "per-task"},
		},
		MemoryComplex: "h_vmem",
	}

	run := func(argv ...string) (string, int) {
		opt, err := parseFlags(argv, false, io.Discard)
		Expect(err).NotTo(HaveOccurred())
		var errBuf bytes.Buffer
		code := runInteractive(cfg, opt, &errBuf)
		return errBuf.String(), code
	}

	It("dry-runs the qrsh line without submitting", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		out, code := run("--pty", "-c", "2", "bash")
		Expect(code).To(Equal(0))
		Expect(out).To(ContainSubstring("would run"))
		Expect(out).To(ContainSubstring("-now no -pty y -cwd -q all.q -pe make 2"))
		Expect(out).To(ContainSubstring("bash"))
	})

	It("redacts -v secret values in the dry run", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		out, _ := run("--pty", "--export", "ALL,HF_TOKEN=secret", "bash")
		Expect(out).To(ContainSubstring("HF_TOKEN=<value>"))
		Expect(out).NotTo(ContainSubstring("secret"))
	})

	It("refuses a command beginning with '-' (qrsh would read it as an option)", func() {
		// pflag cannot itself hand us such a command (a leading-dash token is read
		// as a flag), so exercise the guard directly with a constructed command.
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		opt := &options{pty: true, command: []string{"-x", "y"}, exportSpec: "ALL"}
		var errBuf bytes.Buffer
		code := runInteractive(cfg, opt, &errBuf)
		Expect(code).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("begins with '-'"))
	})

	It("errors when no partition and no default is configured", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		bare := &config.Config{Partitions: map[string]config.Partition{}}
		opt, err := parseFlags([]string{"--pty", "bash"}, false, io.Discard)
		Expect(err).NotTo(HaveOccurred())
		var errBuf bytes.Buffer
		code := runInteractive(bare, opt, &errBuf)
		Expect(code).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("no partition specified"))
	})
})
