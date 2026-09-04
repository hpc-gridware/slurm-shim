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

	It("refuses to write PE-mode state with TMPDIR unset and leaves no sentinel [REQ-FAB-010]", func() {
		GinkgoT().Setenv("TMPDIR", "")
		errBuf := &bytes.Buffer{}
		code := envcmd.Run(nil, io.Discard, errBuf)
		Expect(code).To(Equal(0), "start_proc_args must still exit 0 so the queue instance stays out of E state")
		Expect(errBuf.String()).To(ContainSubstring("TMPDIR is not set"))
		_, err := os.Stat("/tmp/slurm_shim/environment.failed")
		Expect(os.IsNotExist(err)).To(BeTrue(), "no sentinel may be planted at a shared fallback path")
	})

	It("still exports in wrapper mode with TMPDIR unset (no state dir needed) [REQ-FAB-002]", func() {
		GinkgoT().Setenv("TMPDIR", "")
		out := &bytes.Buffer{}
		Expect(envcmd.Run([]string{"--export"}, out, io.Discard)).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("export SLURM_JOB_ID='77'"))
	})

	It("evicts a planted sentinel from the state dir path before fabricating [REQ-FAB-010]", func() {
		dir := filepath.Join(tmp, "slurm_shim")
		Expect(os.Mkdir(dir, 0o777)).To(Succeed())
		Expect(os.Chmod(dir, 0o777)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "environment.failed"), []byte("planted"), 0o666)).To(Succeed())
		Expect(envcmd.Run(nil, io.Discard, io.Discard)).To(Equal(0))
		_, err := os.Stat(filepath.Join(dir, "environment.failed"))
		Expect(os.IsNotExist(err)).To(BeTrue(), "the planted sentinel must be gone")
		_, err = os.Stat(filepath.Join(dir, "environment"))
		Expect(err).NotTo(HaveOccurred(), "and the real environment must have been written")
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

	// A config failure must go through the same sentinel path as any other failure.
	// This hook IS the PE start_proc_args: a non-zero exit puts the queue instance
	// into E state and disables it for every user on the host.
	It("writes the sentinel and exits 0 on a malformed config file [REQ-CFG-002]", func() {
		bad := filepath.Join(tmp, "bad.yaml")
		Expect(os.WriteFile(bad, []byte("launcher: [unclosed\n"), 0o600)).To(Succeed())
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", bad)

		var errOut bytes.Buffer
		Expect(envcmd.Run(nil, io.Discard, &errOut)).To(Equal(0))

		Expect(errOut.String()).To(ContainSubstring("slurm-shim-env: error:"))
		_, err := os.Stat(filepath.Join(tmp, "slurm_shim", "environment.failed"))
		Expect(err).NotTo(HaveOccurred(), "the sentinel is what makes the job fail loudly")
	})

	// Wrapper mode has no sentinel: the job's `eval` needs the exit 1 token, or it
	// runs on with no SLURM_* environment at all.
	It("emits the exit-1 token in export mode on a config failure [REQ-FAB-008]", func() {
		bad := filepath.Join(tmp, "bad.yaml")
		Expect(os.WriteFile(bad, []byte("launcher: [unclosed\n"), 0o600)).To(Succeed())
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", bad)

		var out bytes.Buffer
		Expect(envcmd.Run([]string{"--export"}, &out, io.Discard)).To(Equal(1))
		Expect(strings.TrimSpace(out.String())).To(Equal("exit 1"))
	})

	// A bad slots rule on one partition must not reach this hook at all: it is a
	// submit-time concern, and jobs already queued still need their environment.
	It("fabricates normally despite an unusable slots rule on some partition", func() {
		cfgPath := filepath.Join(tmp, "slots.yaml")
		Expect(os.WriteFile(cfgPath,
			[]byte("partitions:\n  legacy: {queue: a.q, pe: p, slots: \"0\"}\n"), 0o600)).To(Succeed())
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfgPath)

		var errOut bytes.Buffer
		Expect(envcmd.Run(nil, io.Discard, &errOut)).To(Equal(0))
		Expect(errOut.String()).To(ContainSubstring("warning"))
		_, err := os.Stat(filepath.Join(tmp, "slurm_shim", "environment.failed"))
		Expect(os.IsNotExist(err)).To(BeTrue(), "a warning must not fail the job")
	})

	It("removes the state directory on --cleanup [REQ-LCY-002]", func() {
		Expect(envcmd.Run(nil, io.Discard, io.Discard)).To(Equal(0))
		Expect(filepath.Join(tmp, "slurm_shim")).To(BeADirectory())
		Expect(envcmd.Run([]string{"--cleanup"}, io.Discard, io.Discard)).To(Equal(0))
		_, err := os.Stat(filepath.Join(tmp, "slurm_shim"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})
})
