package sbatch

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func TestSbatch(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Sbatch Suite")
}

func testCfg() *config.Config {
	c := config.Default()
	c.Partitions = map[string]config.Partition{
		"gpu":   {Queue: "gpu.q", PE: "gpu.pe", Slots: "per-task"},
		"batch": {Queue: "all.q", PE: "smp.pe", Slots: "16"},
	}
	return c
}

var _ = Describe("#SBATCH directive parsing [REQ-SBT-001]", func() {
	It("extracts directive tokens from the top of the script", func() {
		script := "#!/bin/bash\n#SBATCH --partition=gpu --nodes=2\n#SBATCH -n 8\n\nsrun hostname\n#SBATCH --late=ignored\n"
		Expect(ParseDirectives([]byte(script))).To(Equal([]string{"--partition=gpu", "--nodes=2", "-n", "8"}))
	})

	It("allows blank lines and ordinary comments between directives", func() {
		script := "#!/bin/bash\n# a comment\n#SBATCH -p batch\n\n#SBATCH -N 1\necho hi\n"
		Expect(ParseDirectives([]byte(script))).To(Equal([]string{"-p", "batch", "-N", "1"}))
	})
})

var _ = Describe("sbatch flag parsing [REQ-SBT-001]", func() {
	It("parses known long and short flags", func() {
		opt, warns, err := parseFlags([]string{"--partition=gpu", "-N", "2", "--ntasks-per-node=4", "-c", "2", "job.sh", "arg1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(warns).To(BeEmpty())
		Expect(opt.partition).To(Equal("gpu"))
		Expect(opt.nodes).To(Equal(2))
		Expect(opt.ntasksPerNode).To(Equal(4))
		Expect(opt.cpusPerTask).To(Equal(2))
		Expect(opt.script).To(Equal("job.sh"))
		Expect(opt.scriptArgs).To(Equal([]string{"arg1"}))
	})

	It("warns and ignores unknown directives including --container-* [REQ-SBT-001]", func() {
		opt, warns, err := parseFlags([]string{"--container-image=foo:latest", "--weird", "-p", "batch", "job.sh"})
		Expect(err).NotTo(HaveOccurred())
		Expect(warns).To(ContainElement(ContainSubstring("--container-image")))
		Expect(warns).To(ContainElement(ContainSubstring("--weird")))
		Expect(opt.partition).To(Equal("batch"))
		Expect(opt.script).To(Equal("job.sh"))
	})

	It("consumes a space-form value-taking --container-* flag so it is not taken as the script", func() {
		opt, _, err := parseFlags([]string{"--container-image", "foo:latest", "-p", "batch", "job.sh"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.script).To(Equal("job.sh"))
	})

	It("does not consume a token after a boolean --container-* flag", func() {
		opt, _, err := parseFlags([]string{"--container-readonly", "-p", "batch", "job.sh"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.partition).To(Equal("batch"))
		Expect(opt.script).To(Equal("job.sh"))
	})
})

var _ = Describe("quote-aware directive tokenizing [REQ-SBT-001]", func() {
	It("keeps a quoted value with spaces as one token", func() {
		script := "#!/bin/bash\n#SBATCH --job-name=\"my job\" -p gpu\nsrun x\n"
		Expect(ParseDirectives([]byte(script))).To(Equal([]string{"--job-name=my job", "-p", "gpu"}))
		opt, _, err := parseFlags(ParseDirectives([]byte(script)))
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.jobName).To(Equal("my job"))
	})

	It("handles single quotes", func() {
		Expect(tokenizeDirective("--comment 'hello world' -N 2")).To(Equal([]string{"--comment", "hello world", "-N", "2"}))
	})
})

var _ = Describe("wrapper shell escaping [REQ-SBT-004]", func() {
	It("escapes single quotes in embedded paths", func() {
		Expect(shellQuote("a'b")).To(Equal(`'a'\''b'`))
		w := buildWrapper("/opt/sh'im", "/a/b'c.sh")
		// The stored path is single-quote-escaped, so no bare quote breaks out.
		Expect(w).To(ContainSubstring(`exec '/a/b'\''c.sh'`))
		Expect(w).To(ContainSubstring(`'/opt/sh'\''im'`))
	})
})

var _ = Describe("slots rule + job id [REQ-SBT-002, REQ-SBT-003]", func() {
	It("computes per-task slots as ntasks x cpus_per_task", func() {
		opt, _, _ := parseFlags([]string{"-N", "2", "--ntasks-per-node=4", "-c", "2"})
		slots, err := computeSlots(opt, config.Partition{Slots: "per-task"})
		Expect(err).NotTo(HaveOccurred())
		Expect(slots).To(Equal(16)) // (2*4) tasks * 2 cpus
	})

	It("uses a literal integer slots rule verbatim", func() {
		opt, _, _ := parseFlags([]string{"-N", "2"})
		slots, err := computeSlots(opt, config.Partition{Slots: "16"})
		Expect(err).NotTo(HaveOccurred())
		Expect(slots).To(Equal(16))
	})

	It("defaults -N without -n to one task per node", func() {
		opt, _, _ := parseFlags([]string{"-N", "3"})
		slots, _ := computeSlots(opt, config.Partition{Slots: "per-task"})
		Expect(slots).To(Equal(3))
	})

	DescribeTable("parses the base id from qsub -terse",
		func(terse, want string) { Expect(parseJobID(terse)).To(Equal(want)) },
		Entry("plain", "4711\n", "4711"),
		Entry("array", "4711.1-4:1\n", "4711"),
		Entry("trailing space", "  4712  ", "4712"),
	)
})

// fakeQsub captures the qsub argv and returns a terse id.
func fakeQsub(id string, capture *[]string) *fake.Runner {
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		Expect(name).To(Equal("qsub"))
		*capture = append([]string{}, args...)
		return fake.Response{Stdout: []byte(id + "\n")}
	}}
}

var _ = Describe("sbatch end-to-end [REQ-SBT-002/003/005]", func() {
	writeScript := func(body string) string {
		path := filepath.Join(GinkgoT().TempDir(), "job.sh")
		Expect(os.WriteFile(path, []byte(body), 0o700)).To(Succeed())
		return path
	}

	It("translates a partition to -q/-pe/slots and prints the SLURM line", func() {
		script := writeScript("#!/bin/bash\n#SBATCH --partition=gpu\n#SBATCH -N 2 --ntasks-per-node=4\nsrun hostname\n")
		var captured []string
		var out bytes.Buffer
		rc := run(fakeQsub("4711", &captured), testCfg(), "/shim", []string{script}, &out, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(out.String()).To(Equal("Submitted batch job 4711\n"))
		Expect(captured).To(ContainElements("-terse", "-q", "gpu.q", "-pe", "gpu.pe", "8"))
		Expect(captured[len(captured)-1]).To(Equal(script)) // PE mode submits script as-is
	})

	It("lets a command-line partition override the directive", func() {
		script := writeScript("#!/bin/bash\n#SBATCH --partition=gpu\nsrun hostname\n")
		var captured []string
		rc := run(fakeQsub("5", &captured), testCfg(), "/shim", []string{"-p", "batch", script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(captured).To(ContainElements("-q", "all.q", "-pe", "smp.pe", "16"))
	})

	It("falls back to default_partition when none is given", func() {
		script := writeScript("#!/bin/bash\nsrun hostname\n")
		cfg := testCfg()
		cfg.DefaultPartition = "batch"
		var captured []string
		rc := run(fakeQsub("7", &captured), cfg, "/shim", []string{script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(captured).To(ContainElements("-q", "all.q", "-pe", "smp.pe", "16"))
	})

	It("errors when no partition is given and no default is configured", func() {
		script := writeScript("#!/bin/bash\nsrun hostname\n")
		var captured []string
		var errBuf bytes.Buffer
		rc := run(fakeQsub("8", &captured), testCfg(), "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("no partition specified"))
	})

	It("lets an explicit -p win over default_partition", func() {
		script := writeScript("#!/bin/bash\nsrun hostname\n")
		cfg := testCfg()
		cfg.DefaultPartition = "batch"
		var captured []string
		rc := run(fakeQsub("9", &captured), cfg, "/shim", []string{"-p", "gpu", script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(captured).To(ContainElements("-q", "gpu.q", "-pe", "gpu.pe")) // default ignored
	})

	It("errors with 'unknown partition' when default_partition is invalid", func() {
		script := writeScript("#!/bin/bash\nsrun hostname\n")
		cfg := testCfg()
		cfg.DefaultPartition = "ghost"
		var captured []string
		var errBuf bytes.Buffer
		rc := run(fakeQsub("10", &captured), cfg, "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("unknown partition"))
	})

	It("surfaces a qsub failure and exits non-zero [REQ-SBT-005]", func() {
		script := writeScript("#!/bin/bash\n#SBATCH -p gpu\n")
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qsub: no such PE")}
		}}
		var errBuf bytes.Buffer
		rc := run(r, testCfg(), "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("no such PE"))
	})

	It("errors on an unknown partition", func() {
		script := writeScript("#!/bin/bash\n#SBATCH -p nope\n")
		var errBuf bytes.Buffer
		rc := run(&fake.Runner{}, testCfg(), "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("unknown partition"))
	})

	It("submits a wrapper that execs the stored script verbatim in wrapper mode [REQ-SBT-004]", func() {
		body := "#!/usr/bin/env python\nprint('hi')\n"
		script := writeScript(body)
		cfg := testCfg()
		cfg.WrapperMode = true
		var captured []string
		rc := run(fakeQsub("9", &captured), cfg, "/opt/shim", []string{"-p", "batch", script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		submitted := captured[len(captured)-1]
		Expect(filepath.Base(submitted)).To(Equal("wrapper.sh"))
		w, err := os.ReadFile(submitted)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(w)).To(ContainSubstring("slurm-shim-env --export"))
		Expect(string(w)).To(ContainSubstring("exec '"))
		// The stored original is byte-identical to the user script.
		stored := filepath.Join(filepath.Dir(submitted), "orig.sh")
		got, err := os.ReadFile(stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(body))
	})
})
