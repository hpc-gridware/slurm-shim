package configcmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

func TestConfigCmd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ConfigCmd Suite")
}

var _ = Describe("config check", func() {
	check := func(yaml string) (int, CheckResult) {
		var out, errOut bytes.Buffer
		code := Run([]string{"check", "-", "--json"}, strings.NewReader(yaml), &out, &errOut)
		var res CheckResult
		Expect(json.Unmarshal(out.Bytes(), &res)).To(Succeed(), out.String()+errOut.String())
		Expect(res.SchemaVersion).To(Equal(1))
		return code, res
	}

	DescribeTable("agrees with what a job does with the same file",
		func(yaml string, wantCode int, wantErr, wantWarn string) {
			code, res := check(yaml)
			Expect(code).To(Equal(wantCode))
			Expect(res.Valid).To(Equal(wantCode == 0))
			if wantErr == "" {
				Expect(res.Errors).To(BeEmpty())
			} else {
				Expect(res.Errors).To(ContainElement(ContainSubstring(wantErr)))
			}
			if wantWarn == "" {
				Expect(res.Warnings).To(BeEmpty())
			} else {
				Expect(res.Warnings).To(ContainElement(ContainSubstring(wantWarn)))
			}
		},
		Entry("valid", "default_partition: batch\npartitions:\n  batch: {queue: all.q, pe: smp, slots: \"4\"}\n",
			0, "", ""),
		Entry("malformed YAML", "partitions: [\n", 1, "config:", ""),
		Entry("bad duration", "ping_interval: 5x\n", 1, "invalid duration", ""),
		Entry("unknown key is a warning", "no_such_key: 1\n", 0, "", "no_such_key"),
		Entry("retired key is a warning", "launch_ramp: 8\n", 0, "", "obsolete"),
		Entry("bad slots rule is a warning, as it is for a job",
			"partitions:\n  p: {queue: all.q, pe: smp, slots: \"0\"}\n", 0, "", "slots"),
	)

	It("reads a file and prints a human verdict without --json", func() {
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(path, []byte("ping_interval: 5x\n"), 0o644)).To(Succeed())
		var out, errOut bytes.Buffer
		Expect(Run([]string{"check", path}, nil, &out, &errOut)).To(Equal(1))
		Expect(out.String()).To(ContainSubstring("invalid"))
	})

	It("is a usage error for a missing file", func() {
		var out, errOut bytes.Buffer
		Expect(Run([]string{"check", "/no/such/file"}, nil, &out, &errOut)).To(Equal(2))
	})

	It("refuses an oversized input instead of reading it all", func() {
		var out, errOut bytes.Buffer
		big := strings.NewReader(strings.Repeat("#", maxConfigBytes+1))
		Expect(Run([]string{"check", "-"}, big, &out, &errOut)).To(Equal(2))
		Expect(errOut.String()).To(ContainSubstring("larger than"))
	})
})

var _ = Describe("config path", func() {
	var root, prefix, cellPath string

	BeforeEach(func() {
		root = GinkgoT().TempDir()
		prefix = filepath.Join(root, "slurm-shim")
		Expect(os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(prefix, "bin", "slurm-shim"), []byte("bin"), 0o755)).To(Succeed())
		GinkgoT().Setenv("SGE_ROOT", root)
		GinkgoT().Setenv("SGE_CELL", "default")
		GinkgoT().Setenv(config.EnvVar, "")
		cellPath = filepath.Join(root, "default", config.CellRelPath)
	})

	path := func() (int, PathResult) {
		var out bytes.Buffer
		code := runPath(prefix, true, &out)
		var res PathResult
		Expect(json.Unmarshal(out.Bytes(), &res)).To(Succeed(), out.String())
		Expect(res.SchemaVersion).To(Equal(1))
		return code, res
	}
	write := func(p string) {
		Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
		Expect(os.WriteFile(p, []byte("a: 1\n"), 0o644)).To(Succeed())
	}

	It("reports the defaults when there is no config anywhere", func() {
		code, res := path()
		Expect(code).To(Equal(0))
		Expect(res.Source).To(Equal(config.SourceDefaults))
		Expect(res.Exists).To(BeFalse())
	})

	It("reports the cell config", func() {
		write(cellPath)
		_, res := path()
		Expect(res.Source).To(Equal(config.SourceCell))
		Expect(res.Path).To(Equal(cellPath))
		Expect(res.Exists).To(BeTrue())
	})

	It("reports the pointer and its target", func() {
		shared := filepath.Join(root, "q", "slurm-shim", "config.yaml")
		write(shared)
		Expect(config.WritePointer(prefix, shared)).To(Succeed())
		_, res := path()
		Expect(res.Source).To(Equal(config.SourcePointer))
		Expect(res.Path).To(Equal(shared))
		Expect(res.Pointer).To(Equal(filepath.Join(prefix, config.PointerRel)))
	})

	It("reports $SLURM_SHIM_CONFIG as the source and still names the pointer", func() {
		amd := filepath.Join(root, "config-amd.yaml")
		write(amd)
		Expect(config.WritePointer(prefix, filepath.Join(root, "x.yaml"))).To(Succeed())
		GinkgoT().Setenv(config.EnvVar, amd)
		_, res := path()
		Expect(res.Source).To(Equal(config.SourceEnv))
		Expect(res.Path).To(Equal(amd))
		Expect(res.Pointer).NotTo(BeEmpty())
	})

	It("prints one human line without --json", func() {
		var out bytes.Buffer
		Expect(runPath(prefix, false, &out)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("compiled-in defaults"))

		shared := filepath.Join(root, "q", "slurm-shim", "config.yaml")
		write(shared)
		Expect(config.WritePointer(prefix, shared)).To(Succeed())
		out.Reset()
		Expect(runPath(prefix, false, &out)).To(Equal(0))
		Expect(out.String()).To(Equal(shared + " (pointer " + filepath.Join(prefix, config.PointerRel) + ")\n"))

		Expect(os.Remove(shared)).To(Succeed())
		out.Reset()
		Expect(runPath(prefix, false, &out)).To(Equal(1))
		Expect(out.String()).To(HavePrefix("error: config pointer"))
	})

	It("fails with the reason on a dangling pointer", func() {
		Expect(config.WritePointer(prefix, filepath.Join(root, "gone.yaml"))).To(Succeed())
		code, res := path()
		Expect(code).To(Equal(1))
		Expect(res.Error).To(ContainSubstring("gone.yaml"))
	})
})

var _ = Describe("usage", func() {
	DescribeTable("is exit 2",
		func(args ...string) {
			var out, errOut bytes.Buffer
			Expect(Run(args, nil, &out, &errOut)).To(Equal(2))
		},
		Entry("no subcommand"),
		Entry("unknown subcommand", "show"),
		Entry("check without a file", "check"),
		Entry("path with an argument", "path", "x"),
	)
})
