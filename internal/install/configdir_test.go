package install_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/install"
)

var _ = Describe("Qontrol config dir", func() {
	var dir string

	BeforeEach(func() {
		// A directory Qontrol has initialized: it carries the marker.
		dir = filepath.Join(GinkgoT().TempDir(), "qontrol")
		Expect(os.Mkdir(dir, 0o755)).To(Succeed())
		Expect(os.Chmod(dir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, install.QontrolMarker), []byte("{}"), 0o644)).To(Succeed())
	})

	check := func() install.ConfigDir {
		cd, err := install.CheckConfigDir(dir, "", os.Geteuid())
		Expect(err).NotTo(HaveOccurred())
		return cd
	}

	Describe("CheckConfigDir", func() {
		It("accepts a Qontrol config dir owned by the installing user", func() {
			cd := check()
			Expect(cd.Config).To(Equal(filepath.Join(dir, "slurm-shim", "config.yaml")))
			Expect(cd.UID).To(Equal(os.Geteuid()))
		})

		It("accepts root, which hands the config to the dir's owner", func() {
			_, err := install.CheckConfigDir(dir, "", 0)
			Expect(err).NotTo(HaveOccurred())
		})

		DescribeTable("refuses",
			func(mutate func() (string, string, int), want string) {
				d, cellCommon, euid := mutate()
				_, err := install.CheckConfigDir(d, cellCommon, euid)
				Expect(err).To(MatchError(ContainSubstring(want)))
			},
			Entry("a relative path", func() (string, string, int) {
				return "qontrol", "", os.Geteuid()
			}, "not an absolute path"),
			Entry("characters Qontrol refuses", func() (string, string, int) {
				return "/srv/qontrol config", "", os.Geteuid()
			}, "characters"),
			Entry("a directory Qontrol has not initialized", func() (string, string, int) {
				Expect(os.Remove(filepath.Join(dir, install.QontrolMarker))).To(Succeed())
				return dir, "", os.Geteuid()
			}, "not a Qontrol config dir"),
			Entry("a world-writable directory", func() (string, string, int) {
				Expect(os.Chmod(dir, 0o777)).To(Succeed())
				return dir, "", os.Geteuid()
			}, "writable by group or others"),
			Entry("a group-writable directory", func() (string, string, int) {
				Expect(os.Chmod(dir, 0o775)).To(Succeed())
				return dir, "", os.Geteuid()
			}, "writable by group or others"),
			Entry("a group-writable, non-sticky directory above it", func() (string, string, int) {
				parent := filepath.Join(GinkgoT().TempDir(), "shared")
				Expect(os.Mkdir(parent, 0o755)).To(Succeed())
				d := filepath.Join(parent, "qontrol")
				Expect(os.Mkdir(d, 0o755)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(d, install.QontrolMarker), []byte("{}"), 0o644)).To(Succeed())
				Expect(os.Chmod(parent, 0o775)).To(Succeed())
				return d, "", os.Geteuid()
			}, "and not sticky"),
			Entry("a directory inside the cell's common directory", func() (string, string, int) {
				return dir, filepath.Dir(dir), os.Geteuid()
			}, "inside the cell's common directory"),
			Entry("a non-root installer that does not own the directory", func() (string, string, int) {
				return dir, "", os.Geteuid() + 4242
			}, "is owned by"),
			Entry("a slurm-shim entry that is not a directory", func() (string, string, int) {
				Expect(os.WriteFile(filepath.Join(dir, "slurm-shim"), nil, 0o644)).To(Succeed())
				return dir, "", os.Geteuid()
			}, "not a directory"),
			Entry("a slurm-shim directory others can write", func() (string, string, int) {
				Expect(os.Mkdir(filepath.Join(dir, "slurm-shim"), 0o755)).To(Succeed())
				Expect(os.Chmod(filepath.Join(dir, "slurm-shim"), 0o777)).To(Succeed())
				return dir, "", os.Geteuid()
			}, "make it 0755"),
		)

		It("accepts a sticky world-writable directory above it, like /tmp", func() {
			parent := filepath.Join(GinkgoT().TempDir(), "tmp")
			Expect(os.Mkdir(parent, 0o755)).To(Succeed())
			d := filepath.Join(parent, "qontrol")
			Expect(os.Mkdir(d, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(d, install.QontrolMarker), []byte("{}"), 0o644)).To(Succeed())
			Expect(os.Chmod(parent, os.ModeSticky|0o777)).To(Succeed())
			_, err := install.CheckConfigDir(d, "", os.Geteuid())
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("reading and writing through the dir", func() {
		It("creates slurm-shim/ and a config every job user can read, and reads it back", func() {
			cd := check()
			Expect(cd.WriteConfig([]byte("a: 1\n"))).To(Succeed())
			fi, err := os.Stat(cd.Config)
			Expect(err).NotTo(HaveOccurred())
			Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o644)))
			Expect(cd.ReadConfig()).To(Equal([]byte("a: 1\n")))
		})

		It("reports a missing config as not existing", func() {
			_, err := check().ReadConfig()
			Expect(errors.Is(err, fs.ErrNotExist)).To(BeTrue())
		})

		It("narrows a mode its owner widened, and keeps one narrowed", func() {
			cd := check()
			Expect(cd.WriteConfig([]byte("a: 1\n"))).To(Succeed())
			Expect(os.Chmod(cd.Config, 0o777)).To(Succeed())
			Expect(cd.WriteConfig([]byte("a: 2\n"))).To(Succeed())
			fi, _ := os.Stat(cd.Config)
			Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o644)))

			Expect(os.Chmod(cd.Config, 0o600)).To(Succeed())
			Expect(cd.WriteConfig([]byte("a: 3\n"))).To(Succeed())
			fi, _ = os.Stat(cd.Config)
			Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o600)))
		})

		It("never writes outside the dir when slurm-shim/ is swapped for a symlink after the check", func() {
			cd := check()
			outside := GinkgoT().TempDir()
			Expect(os.Symlink(outside, filepath.Join(dir, "slurm-shim"))).To(Succeed())
			Expect(cd.WriteConfig([]byte("a: 1\n"))).NotTo(Succeed())
			Expect(filepath.Join(outside, "config.yaml")).NotTo(BeAnExistingFile())
		})

		It("refuses to read a config that is a symlink out of the dir", func() {
			cd := check()
			secret := filepath.Join(GinkgoT().TempDir(), "secret")
			Expect(os.WriteFile(secret, []byte("s: 1\n"), 0o600)).To(Succeed())
			Expect(os.Mkdir(filepath.Join(dir, "slurm-shim"), 0o755)).To(Succeed())
			Expect(os.Symlink(secret, cd.Config)).To(Succeed())
			_, err := cd.ReadConfig()
			Expect(err).To(HaveOccurred())
		})

		It("refuses to read a config hardlinked in from elsewhere", func() {
			cd := check()
			other := filepath.Join(dir, "other")
			Expect(os.WriteFile(other, []byte("s: 1\n"), 0o644)).To(Succeed())
			Expect(os.Mkdir(filepath.Join(dir, "slurm-shim"), 0o755)).To(Succeed())
			Expect(os.Link(other, cd.Config)).To(Succeed())
			_, err := cd.ReadConfig()
			Expect(err).To(MatchError(ContainSubstring("one link")))
		})

		It("removes the config and the then-empty slurm-shim/, but keeps a dir with a site's own files", func() {
			cd := check()
			Expect(cd.WriteConfig([]byte("a: 1\n"))).To(Succeed())
			Expect(cd.RemoveConfig()).To(Succeed())
			Expect(filepath.Join(dir, "slurm-shim")).NotTo(BeADirectory())

			Expect(cd.WriteConfig([]byte("a: 1\n"))).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "slurm-shim", "notes"), nil, 0o644)).To(Succeed())
			Expect(cd.RemoveConfig()).To(Succeed())
			Expect(cd.Config).NotTo(BeAnExistingFile())
			Expect(filepath.Join(dir, "slurm-shim", "notes")).To(BeARegularFile())
		})

		It("never deletes outside the dir when slurm-shim/ is a symlink", func() {
			cd := check()
			outside := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(outside, "config.yaml"), []byte("x"), 0o644)).To(Succeed())
			Expect(os.Symlink(outside, filepath.Join(dir, "slurm-shim"))).To(Succeed())
			_ = cd.RemoveConfig()
			Expect(filepath.Join(outside, "config.yaml")).To(BeARegularFile())
		})
	})

	It("recognises a config inside a Qontrol config dir, and nothing else", func() {
		cd, ok := install.OpenConfigDir(filepath.Join(dir, "slurm-shim", "config.yaml"), os.Geteuid())
		Expect(ok).To(BeTrue())
		Expect(cd.Dir).To(Equal(dir))
		_, ok = install.OpenConfigDir(filepath.Join(dir, "other", "config.yaml"), os.Geteuid())
		Expect(ok).To(BeFalse())
		_, ok = install.OpenConfigDir("/opt/ocs/default/common/slurm-shim/config.yaml", os.Geteuid())
		Expect(ok).To(BeFalse())
	})
})
