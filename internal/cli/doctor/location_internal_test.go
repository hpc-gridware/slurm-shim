package doctor

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/install"
)

var _ = Describe("config location warnings", func() {
	var root, prefix, cellPath, shared string

	BeforeEach(func() {
		root = GinkgoT().TempDir()
		prefix = filepath.Join(root, "slurm-shim")
		Expect(os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(prefix, install.BinaryRel), []byte("bin"), 0o755)).To(Succeed())
		GinkgoT().Setenv("SGE_ROOT", root)
		GinkgoT().Setenv("SGE_CELL", "default")
		GinkgoT().Setenv(config.EnvVar, "")
		cellPath = filepath.Join(root, "default", config.CellRelPath)
		shared = filepath.Join(root, "qontrol", "slurm-shim", "config.yaml")
		for _, p := range []string{cellPath, shared} {
			Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
			Expect(os.WriteFile(p, []byte("default_partition: slurm\n"), 0o644)).To(Succeed())
		}
	})

	resolve := func() config.Location {
		loc, err := config.Resolve(prefix)
		Expect(err).NotTo(HaveOccurred())
		return loc
	}

	It("is silent for a plain cell config", func() {
		Expect(locationWarnings(prefix, resolve())).To(BeEmpty())
	})

	It("warns that a cell config is ignored once the pointer names another file", func() {
		Expect(config.WritePointer(prefix, shared)).To(Succeed())
		w := locationWarnings(prefix, resolve())
		Expect(w).To(ConsistOf(ContainSubstring("do not reach it")))
		// Another install may still read the cell file: never advise removing it.
		Expect(w[0]).NotTo(ContainSubstring("remove"))
	})

	It("warns on a config job users cannot read", func() {
		Expect(os.Chmod(cellPath, 0o600)).To(Succeed())
		Expect(locationWarnings(prefix, resolve())).To(ConsistOf(ContainSubstring("not readable by others")))
	})

	It("warns on a directory above the config that others can write", func() {
		Expect(os.Chmod(filepath.Dir(cellPath), 0o775)).To(Succeed())
		Expect(locationWarnings(prefix, resolve())).To(ConsistOf(ContainSubstring("which holds the config")))
	})

	It("warns when the install record and the pointer disagree", func() {
		Expect(os.Remove(cellPath)).To(Succeed())
		Expect(config.WritePointer(prefix, shared)).To(Succeed())
		Expect(install.WriteState(prefix, &install.State{Prefix: prefix, Config: cellPath})).To(Succeed())
		Expect(locationWarnings(prefix, resolve())).To(ConsistOf(ContainSubstring("install record names")))
	})

	It("warns on a config others can write", func() {
		Expect(os.Chmod(cellPath, 0o664)).To(Succeed())
		Expect(locationWarnings(prefix, resolve())).To(ConsistOf(ContainSubstring("writable by group or others")))
	})
})
