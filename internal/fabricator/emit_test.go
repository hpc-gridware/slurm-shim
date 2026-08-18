package fabricator_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/fabricator"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

var _ = Describe("Emission [REQ-ENV-002]", func() {
	homogeneous := "node001 8 all.q@node001\nnode002 8 all.q@node002\n"

	It("renders the unset preamble before the exports [REQ-ENV-011]", func() {
		r, _ := fab(map[string]string{"JOB_ID": "1", "JOB_NAME": "j", "PE": "mpi.pe"}, homogeneous, testConfig())
		out := r.RenderExports()
		firstExport := strings.Index(out, "export ")
		firstUnset := strings.Index(out, "unset ")
		Expect(firstUnset).To(BeNumerically(">=", 0))
		Expect(firstUnset).To(BeNumerically("<", firstExport))
	})

	It("shell-quotes values so adversarial names cannot break eval [REQ-ENV-002]", func() {
		env := map[string]string{
			"JOB_ID": "1", "PE": "mpi.pe",
			"JOB_NAME":     "my job (v2)",
			"SGE_O_HOST":   "login01",
			"SGE_CWD_PATH": "/home/alice/it's a dir; rm -rf /",
		}
		r, err := fab(env, homogeneous, testConfig())
		Expect(err).NotTo(HaveOccurred())
		out := r.RenderExports()

		// Evaluate the rendered script in a real shell and read back a value
		// that started life containing a quote, space, and a metacharacter.
		script := out + "\nprintf '%s' \"$SLURM_SUBMIT_DIR\"\n"
		got, err := exec.Command("/bin/sh", "-c", script).Output()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal("/home/alice/it's a dir; rm -rf /"))
	})

	It("sanitizes the job name to a shell-safe token [REQ-ENV-002]", func() {
		r, _ := fab(map[string]string{"JOB_ID": "1", "PE": "mpi.pe", "JOB_NAME": "my job (v2)"}, homogeneous, testConfig())
		name := exportMap(r)["SLURM_JOB_NAME"]
		Expect(name).To(Equal("my_job__v2_"))
	})

	It("writes layout.json and the environment file together at mode 0600 [REQ-LAY-004] [REQ-FAB-005]", func() {
		r, _ := fab(map[string]string{"JOB_ID": "1", "PE": "mpi.pe", "JOB_NAME": "j"}, homogeneous, testConfig())
		dir := filepath.Join(GinkgoT().TempDir(), "slurm_shim")
		Expect(fabricator.WriteState(dir, r)).To(Succeed())

		for _, f := range []string{layout.LayoutFile, fabricator.EnvFile} {
			info, err := os.Stat(filepath.Join(dir, f))
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
		}
	})

	It("prepends ShimBinDir to PATH so the job can call srun", func() {
		r, _ := fab(map[string]string{"JOB_ID": "1", "PE": "mpi.pe", "JOB_NAME": "j"}, homogeneous, testConfig())
		r.ShimBinDir = "/opt/slurm shim/bin" // space exercises quoting

		// Evaluate against a known PATH and read it back: the dir is prepended and
		// the original $PATH still expands.
		script := "PATH=/usr/bin\n" + r.RenderExports() + "\nprintf '%s' \"$PATH\"\n"
		got, err := exec.Command("/bin/sh", "-c", script).Output()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal("/opt/slurm shim/bin:/usr/bin"))
	})

	It("omits the PATH line when ShimBinDir is unset", func() {
		r, _ := fab(map[string]string{"JOB_ID": "1", "PE": "mpi.pe"}, homogeneous, testConfig())
		Expect(r.RenderExports()).NotTo(ContainSubstring("export PATH="))
	})

	It("omits the PATH line in scrub-only (disabled) mode even with ShimBinDir set", func() {
		// SLURM_SHIM_DISABLE returns a scrub-only result: only the unset preamble,
		// no exports and no PATH line, whatever ShimBinDir holds (SI-38/SI-56).
		r, _ := fab(map[string]string{"JOB_ID": "1", "PE": "mpi.pe", "SLURM_SHIM_DISABLE": "1"}, homogeneous, testConfig())
		r.ShimBinDir = "/opt/slurm-shim/bin"
		out := r.RenderExports()
		Expect(out).NotTo(ContainSubstring("export PATH="))
		Expect(out).NotTo(ContainSubstring("export "))
	})

	It("does not leave a trailing colon on PATH when the inherited PATH is empty", func() {
		r, _ := fab(map[string]string{"JOB_ID": "1", "PE": "mpi.pe", "JOB_NAME": "j"}, homogeneous, testConfig())
		r.ShimBinDir = "/opt/slurm-shim/bin"
		// Empty PATH must yield just the bin dir (no trailing ':' -> no CWD on PATH).
		script := "PATH=''\n" + r.RenderExports() + "\nprintf '%s' \"$PATH\"\n"
		got, err := exec.Command("/bin/sh", "-c", script).Output()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal("/opt/slurm-shim/bin"))
	})

	It("writes the failure sentinel at mode 0600 [REQ-FAB-009]", func() {
		dir := filepath.Join(GinkgoT().TempDir(), "slurm_shim")
		Expect(fabricator.WriteSentinel(dir)).To(Succeed())
		info, err := os.Stat(filepath.Join(dir, fabricator.SentinelFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})
})
