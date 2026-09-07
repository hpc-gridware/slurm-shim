package install_test

import (
	"os"
	"os/user"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/install"
)

// payload builds a fake payload tree with recognisable contents.
func payload(dir string) {
	Expect(os.MkdirAll(filepath.Join(dir, "bin"), 0o755)).To(Succeed())
	Expect(os.MkdirAll(filepath.Join(dir, "etc"), 0o755)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(dir, install.BinaryRel), []byte("BIN"), 0o755)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(dir, install.StarterRel), []byte("STARTER"), 0o755)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(dir, install.HookRel), []byte("HOOK"), 0o644)).To(Succeed())
}

var _ = Describe("InstallTree", func() {
	It("lays out bin/etc/share, copies the three files, links every command relatively", func() {
		src := GinkgoT().TempDir()
		payload(src)
		prefix := filepath.Join(GinkgoT().TempDir(), "slurm-shim")
		Expect(install.InstallTree(src, prefix)).To(Succeed())

		Expect(os.ReadFile(filepath.Join(prefix, install.BinaryRel))).To(Equal([]byte("BIN")))
		Expect(os.ReadFile(filepath.Join(prefix, install.HookRel))).To(Equal([]byte("HOOK")))
		for _, c := range install.Commands {
			target, err := os.Readlink(filepath.Join(prefix, "bin", c))
			Expect(err).NotTo(HaveOccurred(), c)
			Expect(target).To(Equal("slurm-shim"), "links are relative so bin/ copies as a unit")
		}
		fi, _ := os.Stat(filepath.Join(prefix, install.StarterRel))
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o755)))
		fi, _ = os.Stat(filepath.Join(prefix, install.HookRel))
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o644)))
	})

	It("is a no-op copy when the payload already is the prefix (tarball unpacked in place)", func() {
		dir := GinkgoT().TempDir()
		payload(dir)
		Expect(install.InstallTree(dir, dir)).To(Succeed())
		Expect(os.ReadFile(filepath.Join(dir, install.BinaryRel))).To(Equal([]byte("BIN")))
		_, err := os.Readlink(filepath.Join(dir, "bin", "srun"))
		Expect(err).NotTo(HaveOccurred(), "links still created")
	})

	It("re-running over an existing tree is idempotent", func() {
		src := GinkgoT().TempDir()
		payload(src)
		prefix := filepath.Join(GinkgoT().TempDir(), "p")
		Expect(install.InstallTree(src, prefix)).To(Succeed())
		Expect(install.InstallTree(src, prefix)).To(Succeed())
	})
})

var _ = Describe("CheckTree [starter trust boundary]", func() {
	me := func() string {
		u, err := user.Current()
		Expect(err).NotTo(HaveOccurred())
		return u.Username
	}

	It("passes a clean tree owned by an allowed principal", func() {
		src := GinkgoT().TempDir()
		payload(src)
		prefix := filepath.Join(GinkgoT().TempDir(), "p")
		Expect(install.InstallTree(src, prefix)).To(Succeed())
		// The check walks every ancestor to /. On Linux the temp dir sits under
		// /tmp (1777) and on macOS under /var/folders; both must come back clean,
		// so this also pins that a sticky /tmp is not reported as a hazard.
		Expect(install.CheckTree(prefix, me())).To(BeEmpty())
	})

	It("flags a world-writable directory on the path that is NOT sticky", func() {
		// The hazard the check exists for: anyone can replace the tree's parent,
		// and with it the starter that runs as every job user.
		open := filepath.Join(GinkgoT().TempDir(), "open")
		Expect(os.MkdirAll(open, 0o755)).To(Succeed())
		src := GinkgoT().TempDir()
		payload(src)
		prefix := filepath.Join(open, "p")
		Expect(install.InstallTree(src, prefix)).To(Succeed())
		Expect(os.Chmod(open, 0o777)).To(Succeed())

		// Problems name the resolved path -- the one an admin has to chmod --
		// which on macOS differs from the lexical one (/var -> private/var).
		openReal, err := filepath.EvalSymlinks(open)
		Expect(err).NotTo(HaveOccurred())
		Expect(install.CheckTree(prefix, me())).To(ContainElement(And(
			HaveField("Path", openReal),
			HaveField("Why", ContainSubstring("mode 0777")),
		)))
	})

	It("does not flag the same directory once it is sticky", func() {
		// Same 0777 bits plus the sticky bit: entries can no longer be replaced by
		// other users, so the trust chain holds. The reported mode must say 1777,
		// or a reader cannot tell the two cases apart.
		sticky := filepath.Join(GinkgoT().TempDir(), "sticky")
		Expect(os.MkdirAll(sticky, 0o755)).To(Succeed())
		src := GinkgoT().TempDir()
		payload(src)
		prefix := filepath.Join(sticky, "p")
		Expect(install.InstallTree(src, prefix)).To(Succeed())
		Expect(os.Chmod(sticky, 0o777|os.ModeSticky)).To(Succeed())

		stickyReal, err := filepath.EvalSymlinks(sticky)
		Expect(err).NotTo(HaveOccurred())
		for _, p := range install.CheckTree(prefix, me()) {
			Expect(p.Path).NotTo(Equal(stickyReal), "a sticky world-writable dir is not a substitution hazard")
		}
	})

	It("still flags a world-writable FILE even under a sticky directory", func() {
		// Sticky says nothing about a file's own contents.
		sticky := filepath.Join(GinkgoT().TempDir(), "sticky")
		Expect(os.MkdirAll(sticky, 0o755)).To(Succeed())
		src := GinkgoT().TempDir()
		payload(src)
		prefix := filepath.Join(sticky, "p")
		Expect(install.InstallTree(src, prefix)).To(Succeed())
		Expect(os.Chmod(sticky, 0o777|os.ModeSticky)).To(Succeed())
		Expect(os.Chmod(filepath.Join(prefix, install.StarterRel), 0o777|os.ModeSticky)).To(Succeed())

		Expect(install.CheckTree(prefix, me())).To(ContainElement(
			HaveField("Path", HaveSuffix("slurm-shim-starter"))))
	})

	It("flags a group- or world-writable starter", func() {
		src := GinkgoT().TempDir()
		payload(src)
		prefix := filepath.Join(GinkgoT().TempDir(), "p")
		Expect(install.InstallTree(src, prefix)).To(Succeed())
		Expect(os.Chmod(filepath.Join(prefix, install.StarterRel), 0o777)).To(Succeed())
		probs := install.CheckTree(prefix, me())
		Expect(probs).To(ContainElement(HaveField("Why", ContainSubstring("world-writable"))))
	})

	It("flags an owner outside {root, admin_user}", func() {
		src := GinkgoT().TempDir()
		payload(src)
		prefix := filepath.Join(GinkgoT().TempDir(), "p")
		Expect(install.InstallTree(src, prefix)).To(Succeed())
		if me() == "root" {
			Skip("running as root: every file is allowed")
		}
		probs := install.CheckTree(prefix, "") // admin_user none -> only root qualifies
		Expect(probs).To(ContainElement(HaveField("Why", ContainSubstring("owned by "+me()))))
	})

	It("flags a missing command link", func() {
		src := GinkgoT().TempDir()
		payload(src)
		prefix := filepath.Join(GinkgoT().TempDir(), "p")
		Expect(install.InstallTree(src, prefix)).To(Succeed())
		Expect(os.Remove(filepath.Join(prefix, "bin", "srun"))).To(Succeed())
		Expect(install.CheckTree(prefix, me())).To(ContainElement(HaveField("Path", HaveSuffix("/bin/srun"))))
	})
})

var _ = Describe("AdminUser", func() {
	It("reads admin_user from the cell bootstrap and maps none to empty", func() {
		root := GinkgoT().TempDir()
		common := filepath.Join(root, "default", "common")
		Expect(os.MkdirAll(common, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(common, "bootstrap"), []byte("admin_user              none\ndefault_domain none\n"), 0o644)).To(Succeed())
		Expect(install.AdminUser(root, "default")).To(Equal(""))

		Expect(os.WriteFile(filepath.Join(common, "bootstrap"), []byte("admin_user   sgeadmin\n"), 0o644)).To(Succeed())
		Expect(install.AdminUser(root, "")).To(Equal("sgeadmin"), "empty cell means default")
	})
})

var _ = Describe("Expose", func() {
	It("writes a Tcl modulefile under the prefix that prepends bin to PATH", func() {
		prefix := GinkgoT().TempDir()
		path, err := install.Expose(prefix, install.ExposeModule, "1.2.3")
		Expect(err).NotTo(HaveOccurred())
		Expect(path).To(Equal(filepath.Join(prefix, "share", "modulefiles", "slurm-shim", "1.2.3")))
		body, _ := os.ReadFile(path)
		Expect(string(body)).To(HavePrefix("#%Module1.0"))
		Expect(string(body)).To(ContainSubstring("prepend-path PATH " + filepath.Join(prefix, "bin")))
	})

	It("writes nothing for none and rejects an unknown mode", func() {
		path, err := install.Expose(GinkgoT().TempDir(), install.ExposeNone, "1")
		Expect(err).NotTo(HaveOccurred())
		Expect(path).To(BeEmpty())
		_, err = install.Expose(GinkgoT().TempDir(), "bashrc", "1")
		Expect(err).To(HaveOccurred())
	})
})
