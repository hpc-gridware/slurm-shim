package config_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

var _ = Describe("config location [REQ-CFG-001]", func() {
	var root, prefix, cellPath, shared string

	BeforeEach(func() {
		root = GinkgoT().TempDir()
		prefix = filepath.Join(root, "slurm-shim")
		Expect(os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(prefix, "bin", "slurm-shim"), []byte("bin"), 0o755)).To(Succeed())
		GinkgoT().Setenv("SGE_ROOT", root)
		GinkgoT().Setenv("SGE_CELL", "default")
		GinkgoT().Setenv(config.EnvVar, "")
		cellPath = filepath.Join(root, "default", config.CellRelPath)
		shared = filepath.Join(root, "qontrol", "slurm-shim", "config.yaml")
	})

	write := func(path, content string) {
		Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
		Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
	}

	It("uses the cell config when the install has no pointer", func() {
		write(cellPath, "default_partition: cell\n")
		cfg, loc, _, err := config.LoadFor(prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(loc.Source).To(Equal(config.SourceCell))
		Expect(loc.Path).To(Equal(cellPath))
		Expect(loc.Pointer).To(BeEmpty())
		Expect(cfg.DefaultPartition).To(Equal("cell"))
	})

	It("follows the pointer ahead of the cell config", func() {
		write(cellPath, "default_partition: cell\n")
		write(shared, "default_partition: shared\n")
		Expect(config.WritePointer(prefix, shared)).To(Succeed())

		cfg, loc, _, err := config.LoadFor(prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(loc.Source).To(Equal(config.SourcePointer))
		Expect(loc.Path).To(Equal(shared))
		Expect(loc.Pointer).To(Equal(filepath.Join(prefix, config.PointerRel)))
		Expect(cfg.DefaultPartition).To(Equal("shared"))
	})

	It("lets $SLURM_SHIM_CONFIG override the pointer and still names the pointer", func() {
		write(shared, "default_partition: shared\n")
		amd := filepath.Join(root, "config-amd.yaml")
		write(amd, "default_partition: amd\n")
		Expect(config.WritePointer(prefix, shared)).To(Succeed())
		GinkgoT().Setenv(config.EnvVar, amd)

		cfg, loc, _, err := config.LoadFor(prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(loc.Source).To(Equal(config.SourceEnv))
		Expect(loc.Pointer).NotTo(BeEmpty())
		Expect(cfg.DefaultPartition).To(Equal("amd"))
	})

	It("does not fail a job over a broken pointer that $SLURM_SHIM_CONFIG overrides", func() {
		Expect(config.WritePointer(prefix, filepath.Join(root, "gone.yaml"))).To(Succeed())
		GinkgoT().Setenv(config.EnvVar, filepath.Join(root, "missing.yaml"))
		cfg, loc, _, err := config.LoadFor(prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(loc.Source).To(Equal(config.SourceDefaults))
		Expect(cfg.Partitions).To(BeEmpty())
	})

	It("FAILS on a dangling pointer instead of falling through to the cell config", func() {
		write(cellPath, "default_partition: cell\n")
		gone := filepath.Join(root, "gone", "config.yaml")
		Expect(config.WritePointer(prefix, gone)).To(Succeed())

		_, _, _, err := config.LoadFor(prefix)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(filepath.Join(prefix, config.PointerRel)))
		Expect(err.Error()).To(ContainSubstring(gone))
	})

	DescribeTable("refuses a pointer it cannot trust or parse",
		func(setup func(pointer string), want string) {
			pointer := filepath.Join(prefix, config.PointerRel)
			Expect(os.MkdirAll(filepath.Dir(pointer), 0o755)).To(Succeed())
			setup(pointer)
			_, _, _, err := config.LoadFor(prefix)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(want))
		},
		Entry("a symlink", func(p string) {
			target := filepath.Join(root, "elsewhere")
			Expect(os.WriteFile(target, []byte("/x\n"), 0o644)).To(Succeed())
			Expect(os.Symlink(target, p)).To(Succeed())
		}, "not a regular file"),
		Entry("group-writable", func(p string) {
			Expect(os.WriteFile(p, []byte("/x\n"), 0o644)).To(Succeed())
			Expect(os.Chmod(p, 0o664)).To(Succeed())
		}, "writable by group or others"),
		Entry("a relative path", func(p string) {
			Expect(os.WriteFile(p, []byte("conf/config.yaml\n"), 0o644)).To(Succeed())
		}, "one absolute path"),
		Entry("empty", func(p string) {
			Expect(os.WriteFile(p, []byte("\n"), 0o644)).To(Succeed())
		}, "one absolute path"),
		Entry("two lines", func(p string) {
			Expect(os.WriteFile(p, []byte("/a\n/b\n"), 0o644)).To(Succeed())
		}, "one absolute path"),
	)

	It("FAILS on a pointer target the caller cannot read, naming the pointer", func() {
		write(shared, "default_partition: shared\n")
		Expect(os.Chmod(shared, 0o000)).To(Succeed())
		DeferCleanup(os.Chmod, shared, os.FileMode(0o644))
		Expect(config.WritePointer(prefix, shared)).To(Succeed())
		_, err := config.Resolve(prefix)
		Expect(err).To(MatchError(ContainSubstring(filepath.Join(prefix, config.PointerRel))))
	})

	It("FAILS when the cell directory cannot be searched, instead of falling back to the defaults", func() {
		write(cellPath, "default_partition: cell\n")
		dir := filepath.Dir(cellPath)
		Expect(os.Chmod(dir, 0o000)).To(Succeed())
		DeferCleanup(os.Chmod, dir, os.FileMode(0o755))
		_, err := config.Resolve(prefix)
		Expect(err).To(MatchError(ContainSubstring(cellPath)))
	})

	It("refuses a pointer in a directory others can write", func() {
		Expect(config.WritePointer(prefix, shared)).To(Succeed())
		etc := filepath.Join(prefix, "etc")
		Expect(os.Chmod(etc, 0o775)).To(Succeed())
		_, _, err := config.ReadPointer(prefix)
		Expect(err).To(MatchError(ContainSubstring("its directory")))
	})

	It("writes a pointer every job user can read and reads it back", func() {
		Expect(config.WritePointer(prefix, shared+"/../config.yaml")).To(Succeed())
		fi, err := os.Stat(filepath.Join(prefix, config.PointerRel))
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o644)))
		target, ok, err := config.ReadPointer(prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(target).To(Equal(filepath.Join(root, "qontrol", "slurm-shim", "config.yaml")))
	})

	It("refuses to write a relative pointer", func() {
		Expect(config.WritePointer(prefix, "config.yaml")).NotTo(Succeed())
	})

	It("removes the pointer, and removing none is not an error", func() {
		Expect(config.WritePointer(prefix, shared)).To(Succeed())
		Expect(config.RemovePointer(prefix)).To(Succeed())
		Expect(config.RemovePointer(prefix)).To(Succeed())
		_, ok, err := config.ReadPointer(prefix)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	Describe("InstallPath", func() {
		It("is the cell path without a pointer", func() {
			p, src, err := config.InstallPath(prefix)
			Expect(err).NotTo(HaveOccurred())
			Expect(p).To(Equal(cellPath))
			Expect(src).To(Equal(config.SourceCell))
		})

		It("follows the pointer even when its target is gone, so install can re-create it", func() {
			Expect(config.WritePointer(prefix, shared)).To(Succeed())
			p, src, err := config.InstallPath(prefix)
			Expect(err).NotTo(HaveOccurred())
			Expect(p).To(Equal(shared))
			Expect(src).To(Equal(config.SourcePointer))
		})

		It("keeps $SLURM_SHIM_CONFIG first, as the installer always did", func() {
			GinkgoT().Setenv(config.EnvVar, "/tmp/x.yaml")
			p, src, err := config.InstallPath(prefix)
			Expect(err).NotTo(HaveOccurred())
			Expect(p).To(Equal("/tmp/x.yaml"))
			Expect(src).To(Equal(config.SourceEnv))
		})
	})

	It("CellPath ignores both $SLURM_SHIM_CONFIG and the pointer", func() {
		GinkgoT().Setenv(config.EnvVar, "/tmp/x.yaml")
		Expect(config.WritePointer(prefix, shared)).To(Succeed())
		Expect(config.CellPath()).To(Equal(cellPath))
	})
})
