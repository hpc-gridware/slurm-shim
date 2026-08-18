package env_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	envcmd "github.com/hpc-gridware/slurm-shim/internal/cli/env"
)

func TestEnvCLI(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Env CLI Suite")
}

var _ = Describe("slurm-shim-env command", func() {
	var tmp string

	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		// Force built-in config defaults deterministically.
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", filepath.Join(tmp, "no-config.yaml"))
		// A non-PE single-node job (no PE_HOSTFILE, NHOSTS defaults to 1).
		GinkgoT().Setenv("JOB_ID", "77")
		GinkgoT().Setenv("JOB_NAME", "t")
		GinkgoT().Setenv("SGE_TASK_ID", "undefined")
		GinkgoT().Setenv("NSLOTS", "4")
		GinkgoT().Setenv("PE", "")
		GinkgoT().Setenv("PE_HOSTFILE", "")
	})

	It("prints export lines in wrapper mode [REQ-FAB-002]", func() {
		out := &bytes.Buffer{}
		code := envcmd.Run([]string{"--export"}, out, io.Discard)
		Expect(code).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("unset SLURM_JOB_ID"))
		Expect(out.String()).To(ContainSubstring("export SLURM_JOB_ID='77'"))
		Expect(out.String()).To(ContainSubstring("export SLURM_NNODES='1'"))
		// The command wires ShimBinDir from os.Executable() so the job can find
		// srun/sbatch; a regression there would silently drop this PATH line.
		Expect(out.String()).To(ContainSubstring("export PATH="))
	})

	It("writes layout, environment, and step counter in PE mode [REQ-FAB-001]", func() {
		code := envcmd.Run(nil, io.Discard, io.Discard)
		Expect(code).To(Equal(0))
		for _, f := range []string{"layout.json", "environment", "stepctr"} {
			_, err := os.Stat(filepath.Join(tmp, "slurm_shim", f))
			Expect(err).NotTo(HaveOccurred(), f)
		}
	})

	It("aborts wrapper mode with the sole token exit 1 on failure [REQ-FAB-008]", func() {
		// A present-but-empty PE_HOSTFILE is a fabrication error.
		hf := filepath.Join(tmp, "empty_hostfile")
		Expect(os.WriteFile(hf, []byte(""), 0o600)).To(Succeed())
		GinkgoT().Setenv("PE_HOSTFILE", hf)
		out := &bytes.Buffer{}
		code := envcmd.Run([]string{"--export"}, out, io.Discard)
		Expect(code).To(Equal(1))
		Expect(strings.TrimSpace(out.String())).To(Equal("exit 1"))
	})

	It("writes the sentinel and exits 0 on PE-mode failure [REQ-FAB-009]", func() {
		hf := filepath.Join(tmp, "empty_hostfile")
		Expect(os.WriteFile(hf, []byte(""), 0o600)).To(Succeed())
		GinkgoT().Setenv("PE_HOSTFILE", hf)
		code := envcmd.Run(nil, io.Discard, io.Discard)
		Expect(code).To(Equal(0))
		_, err := os.Stat(filepath.Join(tmp, "slurm_shim", "environment.failed"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("exits 2 on a malformed config file [REQ-CFG-002]", func() {
		bad := filepath.Join(tmp, "bad.yaml")
		Expect(os.WriteFile(bad, []byte("launcher: [unclosed\n"), 0o600)).To(Succeed())
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", bad)
		Expect(envcmd.Run(nil, io.Discard, io.Discard)).To(Equal(2))
	})

	It("removes the state directory on --cleanup [REQ-LCY-002]", func() {
		Expect(envcmd.Run(nil, io.Discard, io.Discard)).To(Equal(0))
		Expect(filepath.Join(tmp, "slurm_shim")).To(BeADirectory())
		Expect(envcmd.Run([]string{"--cleanup"}, io.Discard, io.Discard)).To(Equal(0))
		_, err := os.Stat(filepath.Join(tmp, "slurm_shim"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})
})
