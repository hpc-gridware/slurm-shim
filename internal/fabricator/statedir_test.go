package fabricator_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/fabricator"
)

// The per-job TMPDIR is a predictable, co-tenant-reachable path; these specs pin
// the writing side of the trust boundary (REQ-FAB-010). A foreign OWNER cannot be
// fabricated without root, so the symlink and mode variants stand in for it.
var _ = Describe("EnsureStateDir [REQ-FAB-010]", func() {
	var tmp string

	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
	})

	It("creates the state dir 0700 under a fresh TMPDIR", func() {
		dir, err := fabricator.EnsureStateDir(tmp)
		Expect(err).NotTo(HaveOccurred())
		Expect(dir).To(Equal(filepath.Join(tmp, "slurm_shim")))
		fi, err := os.Lstat(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.IsDir()).To(BeTrue())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o700)))
	})

	It("reuses an existing private state dir and keeps its contents", func() {
		dir := filepath.Join(tmp, "slurm_shim")
		Expect(os.Mkdir(dir, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o600)).To(Succeed())
		got, err := fabricator.EnsureStateDir(tmp)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(dir))
		_, err = os.Stat(filepath.Join(dir, "keep"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("moves a planted symlink aside and creates a real dir", func() {
		target := filepath.Join(tmp, "elsewhere")
		Expect(os.Mkdir(target, 0o700)).To(Succeed())
		dir := filepath.Join(tmp, "slurm_shim")
		Expect(os.Symlink(target, dir)).To(Succeed())
		got, err := fabricator.EnsureStateDir(tmp)
		Expect(err).NotTo(HaveOccurred())
		fi, err := os.Lstat(got)
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode()&os.ModeSymlink).To(BeZero(), "state dir must be a real directory")
		// The planted link survives under another name; nothing was written through it.
		entries, err := os.ReadDir(target)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
	})

	It("moves a group/world-writable state dir aside and creates a private one", func() {
		dir := filepath.Join(tmp, "slurm_shim")
		Expect(os.Mkdir(dir, 0o777)).To(Succeed())
		Expect(os.Chmod(dir, 0o777)).To(Succeed()) // defeat umask
		Expect(os.WriteFile(filepath.Join(dir, "environment.failed"), []byte("planted"), 0o666)).To(Succeed())
		got, err := fabricator.EnsureStateDir(tmp)
		Expect(err).NotTo(HaveOccurred())
		fi, err := os.Lstat(got)
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o700)))
		_, err = os.Stat(filepath.Join(got, "environment.failed"))
		Expect(os.IsNotExist(err)).To(BeTrue(), "the planted sentinel must not survive into the fresh dir")
	})

	It("strips group/world write from a TMPDIR the job owns", func() {
		Expect(os.Chmod(tmp, 0o777)).To(Succeed())
		_, err := fabricator.EnsureStateDir(tmp)
		Expect(err).NotTo(HaveOccurred())
		fi, err := os.Lstat(tmp)
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode().Perm() & 0o022).To(BeZero())
	})

	It("refuses a TMPDIR that is a symlink", func() {
		link := filepath.Join(tmp, "tmplink")
		Expect(os.Symlink(tmp, link)).To(Succeed())
		_, err := fabricator.EnsureStateDir(link)
		Expect(err).To(MatchError(ContainSubstring("symlink")))
	})

	It("refuses an unset TMPDIR rather than falling back to a shared path", func() {
		_, err := fabricator.EnsureStateDir("")
		Expect(err).To(MatchError(ContainSubstring("TMPDIR is not set")))
	})
})
