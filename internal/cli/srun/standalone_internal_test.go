package srun

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// srun without --pty outside an allocation creates one and runs the command as
// a step in it; salloc creates one and runs a command (or the shell) once.
var _ = Describe("srun outside an allocation, without --pty", func() {
	cfg := &config.Config{
		DefaultPartition: "batch",
		Partitions:       map[string]config.Partition{"batch": {Queue: "all.q", PE: "make", Slots: "per-task"}},
	}
	dry := func(argv ...string) (string, int) {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		opt, err := parseFlags(argv, false, io.Discard)
		Expect(err).NotTo(HaveOccurred())
		var errBuf bytes.Buffer
		code := runStandalone(cfg, opt, &errBuf)
		return errBuf.String(), code
	}

	It("runs the shim's own srun with the step options in a session without a terminal", func() {
		out, code := dry("-N", "2", "-n", "4", "-c", "2", "-l", "-o", "out.%t", "python", "train.py", "--epochs", "3")
		Expect(code).To(Equal(0))
		line := out[strings.Index(out, "would run"):]
		Expect(line).To(ContainSubstring("-now no -cwd -q all.q -pe make 8"))
		Expect(line).NotTo(ContainSubstring("-pty"), "an explicit -pty no fails the job")
		self, _ := os.Executable()
		Expect(line).To(ContainSubstring(self + " srun --ntasks=4 --nodes=2 --cpus-per-task=2 --output=out.%t --export=ALL --label -- python train.py --epochs 3"))
	})

	// The inner srun must see every step option the outer one was given: a
	// dropped one changes the step silently.
	DescribeTable("hands every step option on to the inner srun unchanged",
		func(argv ...string) {
			outer, err := parseFlags(append(argv, "hostname"), false, io.Discard)
			Expect(err).NotTo(HaveOccurred())
			inner, err := parseFlags(append(stepArgs(outer), "--", "hostname"), false, io.Discard)
			Expect(err).NotTo(HaveOccurred())
			Expect(inner.req).To(Equal(outer.req))
			Expect(inner.output).To(Equal(outer.output))
			Expect(inner.errorPat).To(Equal(outer.errorPat))
			Expect(inner.killFlag).To(Equal(outer.killFlag))
			Expect(inner.gpuBind).To(Equal(outer.gpuBind))
			Expect(inner.elastic).To(Equal(outer.elastic))
			Expect(inner.exportSpec).To(Equal(outer.exportSpec))
			Expect(inner.label).To(Equal(outer.label))
			Expect(inner.command).To(Equal([]string{"hostname"}))
		},
		Entry("geometry", "-n", "6", "-N", "3", "--ntasks-per-node=2", "-c", "4"),
		Entry("gpus per task", "-N", "2", "--gpus-per-task=2"),
		Entry("gpu binding per task", "-N", "1", "--gpu-bind=per_task:2"),
		Entry("output and error patterns with spaces", "-o", "out %t.log", "-e", "err %t.log"),
		Entry("bare -K", "-K", "-N", "1"),
		Entry("-K=0", "--kill-on-bad-exit=0"),
		Entry("hot spares", "--x-elastic=off"),
		Entry("export list", "--export=ALL,FOO=a b"),
		Entry("label", "-l"),
	)

	It("passes a command beginning with '-' after the -- of the inner srun", func() {
		opt := &options{command: []string{"-x"}, exportSpec: "ALL"}
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		var errBuf bytes.Buffer
		Expect(runStandalone(cfg, opt, &errBuf)).To(Equal(0))
		Expect(errBuf.String()).To(ContainSubstring("-- -x"))
	})

	It("starts at once or fails with -I", func() {
		out, code := dry("-I", "hostname")
		Expect(code).To(Equal(0))
		Expect(out).To(ContainSubstring("-now yes"))
		out, _ = dry("-I30", "hostname")
		Expect(out).To(ContainSubstring("the wait of 30 seconds is not applied"))
	})

	It("accepts --overlap without a word: steps already share the allocation", func() {
		opt, err := parseFlags([]string{"--overlap", "hostname"}, false, io.Discard)
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.warnings).To(BeEmpty())
	})
})

var _ = Describe("salloc", func() {
	BeforeEach(func() {
		cfgFile := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(cfgFile, []byte("default_partition: batch\npartitions:\n  batch: {queue: all.q, pe: make, slots: per-task}\n"), 0o644)).To(Succeed())
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfgFile)
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
	})

	It("runs the user's shell once on the allocation when no command is given", func() {
		GinkgoT().Setenv("SHELL", "/bin/zsh")
		var errBuf bytes.Buffer
		Expect(RunSalloc([]string{"-N", "2", "--ntasks-per-node", "4"}, io.Discard, &errBuf)).To(Equal(0))
		Expect(errBuf.String()).To(ContainSubstring("-q all.q -pe make 8"))
		Expect(errBuf.String()).To(ContainSubstring("/bin/zsh"))
		Expect(errBuf.String()).NotTo(ContainSubstring(" srun "), "salloc runs the command once, not as a step")
	})

	It("runs a given command and parses salloc's own options", func() {
		var errBuf bytes.Buffer
		Expect(RunSalloc([]string{"--bell", "-J", "dbg", "hostname"}, io.Discard, &errBuf)).To(Equal(0))
		Expect(errBuf.String()).To(MatchRegexp(`-N dbg -V hostname\n`))
		Expect(errBuf.String()).NotTo(ContainSubstring("-pty"), "stdin is not a terminal here")
	})

	It("falls back to /bin/sh when SHELL is unset", func() {
		GinkgoT().Setenv("SHELL", "")
		var errBuf bytes.Buffer
		Expect(RunSalloc([]string{"-N", "1"}, io.Discard, &errBuf)).To(Equal(0))
		Expect(errBuf.String()).To(MatchRegexp(`-V /bin/sh\n`))
	})

	It("refuses a command beginning with '-', which qrsh would read as its own option", func() {
		var errBuf bytes.Buffer
		Expect(RunSalloc([]string{"-N", "1", "--", "-x"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(HavePrefix("salloc: "))
	})

	DescribeTable("refuses --no-shell wherever it appears among the options",
		func(args ...string) {
			var errBuf bytes.Buffer
			Expect(RunSalloc(args, io.Discard, &errBuf)).To(Equal(1))
			Expect(errBuf.String()).To(ContainSubstring("--no-shell is not supported"))
		},
		Entry("first", "--no-shell", "-N", "1"),
		Entry("after a flag value", "-N", "2", "--no-shell"),
		Entry("after a long option value", "-p", "batch", "--no-shell", "hostname"),
	)
})
