package version_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/version"
)

var _ = Describe("Version string [REQ-DLV-003]", func() {
	It("renders slurm <compat> (slurm-shim <shim>)", func() {
		version.Shim = "1.2.3"
		Expect(version.String("24.05.0")).To(Equal("slurm 24.05.0 (slurm-shim 1.2.3)"))
	})

	It("falls back to the default compat version when compat is empty [REQ-DLV-003]", func() {
		version.Shim = "1.2.3"
		Expect(version.String("")).To(Equal("slurm " + version.DefaultCompat + " (slurm-shim 1.2.3)"))
	})

	It("is parsable by a SLURM version regex [REQ-DLV-003]", func() {
		version.Shim = "0.1.0-dev"
		Expect(version.String("24.05.0")).To(MatchRegexp(`^slurm \d+\.\d+\.\d+ \(slurm-shim .+\)$`))
	})
})
