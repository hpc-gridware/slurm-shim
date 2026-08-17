package layout_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

var _ = Describe("layout error paths", func() {
	It("reports a descriptive schema-version error [REQ-LAY-005]", func() {
		err := layout.ErrSchemaVersion{Got: 2, Want: 1}
		Expect(err.Error()).To(ContainSubstring("schema_version 2"))
		Expect(err.Error()).To(ContainSubstring("understands 1"))
	})

	It("fails Write when the target directory cannot be created [REQ-LAY-004]", func() {
		dir := GinkgoT().TempDir()
		file := filepath.Join(dir, "afile")
		Expect(os.WriteFile(file, []byte("x"), 0o600)).To(Succeed())
		// A regular file cannot be a parent directory; MkdirAll must fail.
		Expect(layout.Write(filepath.Join(file, "sub"), sample())).NotTo(Succeed())
	})

	It("fails Read on a missing file", func() {
		_, err := layout.Read(filepath.Join(GinkgoT().TempDir(), "absent.json"))
		Expect(err).To(HaveOccurred())
	})

	It("rejects a corrupt step counter [REQ-LCY-003]", func() {
		ctr := filepath.Join(GinkgoT().TempDir(), "stepctr")
		Expect(os.WriteFile(ctr, []byte("not-a-number\n"), 0o600)).To(Succeed())
		_, err := layout.NextStep(ctr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("corrupt step counter"))
	})

	It("fails NextStep when the counter path is unopenable [REQ-LCY-003]", func() {
		// Parent directory does not exist, so O_CREATE cannot open the file.
		ctr := filepath.Join(GinkgoT().TempDir(), "no-such-dir", "stepctr")
		_, err := layout.NextStep(ctr)
		Expect(err).To(HaveOccurred())
	})
})
