package srun

import (
	"io"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// srun used to resolve the per-job state dir to a shared /tmp/slurm_shim when
// TMPDIR was unset, reading layout.json and the step counter -- allocation truth
// -- from a world-known path any local user can plant files in (REQ-FAB-010).
var _ = Describe("srun per-job state resolution [REQ-FAB-010]", func() {
	It("resolves the state dir under a set TMPDIR", func() {
		GinkgoT().Setenv("TMPDIR", "/tmp/77.1.all.q")
		dir, err := stateDir()
		Expect(err).NotTo(HaveOccurred())
		Expect(dir).To(Equal("/tmp/77.1.all.q/slurm_shim"))
	})

	It("refuses an unset TMPDIR instead of falling back to a shared /tmp path", func() {
		GinkgoT().Setenv("TMPDIR", "")
		_, err := stateDir()
		Expect(err).To(MatchError(os.ErrNotExist))
	})

	It("reports the standalone case rather than reading /tmp for the layout", func() {
		GinkgoT().Setenv("TMPDIR", "")
		cfg := &config.Config{Standalone: "reject"}
		_, err := loadLayout(cfg, io.Discard)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not inside a slurm-shim allocation"))
	})
})
