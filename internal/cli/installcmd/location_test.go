package installcmd

import (
	"bytes"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/install"
)

var _ = Describe("config location during install and uninstall", func() {
	var root, cellPath string

	// tree makes a minimal install tree, so TreeMatcher and the pointer's
	// owner check have a binary to look at.
	tree := func(name string) string {
		p := filepath.Join(root, name)
		Expect(os.MkdirAll(filepath.Join(p, "bin"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(p, install.BinaryRel), []byte("bin"), 0o755)).To(Succeed())
		return p
	}
	queueOf := func(prefix string) gedata.Queue {
		return gedata.Queue{Name: filepath.Base(prefix) + ".q", StarterMethod: filepath.Join(prefix, install.StarterRel)}
	}
	// qontrolDir makes a Qontrol config dir with the marker.
	qontrolDir := func() string {
		d := filepath.Join(root, "qontrol")
		Expect(os.MkdirAll(d, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(d, install.QontrolMarker), []byte("{}"), 0o644)).To(Succeed())
		return d
	}

	BeforeEach(func() {
		root = GinkgoT().TempDir()
		GinkgoT().Setenv("SGE_ROOT", root)
		GinkgoT().Setenv("SGE_CELL", "default")
		GinkgoT().Setenv(config.EnvVar, "")
		cellPath = filepath.Join(root, "default", config.CellRelPath)
		Expect(os.MkdirAll(filepath.Dir(cellPath), 0o755)).To(Succeed())
		Expect(os.WriteFile(cellPath, []byte("default_partition: slurm\n"), 0o644)).To(Succeed())
	})

	Describe("resolveConfigTarget", func() {
		It("is the cell config, with no pointer, for a plain install", func() {
			t, _, err := resolveConfigTarget("", "", tree("self"), os.Geteuid())
			Expect(err).NotTo(HaveOccurred())
			Expect(t.Path).To(Equal(cellPath))
			Expect(t.WritePointer).To(BeFalse())
			Expect(t.Previous).To(BeEmpty())
		})

		It("follows and keeps the pointer on a re-run", func() {
			self := tree("self")
			shared := filepath.Join(root, "shared.yaml")
			Expect(config.WritePointer(self, shared)).To(Succeed())
			t, _, err := resolveConfigTarget("", "", self, os.Geteuid())
			Expect(err).NotTo(HaveOccurred())
			Expect(t.Path).To(Equal(shared))
			Expect(t.Source).To(Equal(config.SourcePointer))
			Expect(t.WritePointer).To(BeTrue())
			Expect(t.Previous).To(BeEmpty())
		})

		It("never makes $SLURM_SHIM_CONFIG permanent", func() {
			GinkgoT().Setenv(config.EnvVar, filepath.Join(root, "oneoff.yaml"))
			t, _, err := resolveConfigTarget("", "", tree("self"), os.Geteuid())
			Expect(err).NotTo(HaveOccurred())
			Expect(t.Source).To(Equal(config.SourceEnv))
			Expect(t.WritePointer).To(BeFalse())
		})

		It("carries the current config along when --config moves it", func() {
			t, _, err := resolveConfigTarget(filepath.Join(root, "new.yaml"), "", tree("self"), os.Geteuid())
			Expect(err).NotTo(HaveOccurred())
			Expect(t.WritePointer).To(BeTrue())
			Expect(t.Previous).To(Equal(cellPath))
		})

		It("drops the pointer when --config names the cell path again", func() {
			self := tree("self")
			shared := filepath.Join(root, "shared.yaml")
			Expect(config.WritePointer(self, shared)).To(Succeed())
			t, _, err := resolveConfigTarget(cellPath, "", self, os.Geteuid())
			Expect(err).NotTo(HaveOccurred())
			Expect(t.WritePointer).To(BeFalse())
			Expect(t.Previous).To(Equal(shared))
		})

		It("moves into a Qontrol config dir with --config-dir", func() {
			d := qontrolDir()
			t, _, err := resolveConfigTarget("", d, tree("self"), os.Geteuid())
			Expect(err).NotTo(HaveOccurred())
			Expect(t.Dir).NotTo(BeNil())
			Expect(t.Path).To(Equal(filepath.Join(d, "slurm-shim", "config.yaml")))
			Expect(t.WritePointer).To(BeTrue())
			Expect(t.Previous).To(Equal(cellPath))
		})

		It("treats a re-run that follows the pointer into a config dir like --config-dir", func() {
			self := tree("self")
			d := qontrolDir()
			Expect(config.WritePointer(self, filepath.Join(d, "slurm-shim", "config.yaml"))).To(Succeed())
			t, _, err := resolveConfigTarget("", "", self, os.Geteuid())
			Expect(err).NotTo(HaveOccurred())
			Expect(t.Dir).NotTo(BeNil(), "writes then go through the dir and keep its owner")
		})

		It("is a usage error with both flags", func() {
			_, code, err := resolveConfigTarget("x", "y", tree("self"), os.Geteuid())
			Expect(err).To(HaveOccurred())
			Expect(code).To(Equal(2))
		})

		It("refuses an untrusted pointer before anything changes", func() {
			self := tree("self")
			Expect(config.WritePointer(self, filepath.Join(root, "shared.yaml"))).To(Succeed())
			Expect(os.Chmod(filepath.Join(self, config.PointerRel), 0o666)).To(Succeed())
			_, code, err := resolveConfigTarget("", "", self, os.Geteuid())
			Expect(err).To(MatchError(ContainSubstring("refusing to trust it")))
			Expect(code).To(Equal(1))
		})
	})

	Describe("readerOf", func() {
		It("names another install that reads the cell config by default", func() {
			other := tree("other")
			Expect(readerOf(cellPath, "", install.Facts{Queues: []gedata.Queue{queueOf(other)}})).
				To(ContainSubstring("other.q"))
		})

		It("does not count an install whose pointer names another file", func() {
			other := tree("other")
			Expect(config.WritePointer(other, filepath.Join(root, "elsewhere.yaml"))).To(Succeed())
			Expect(readerOf(cellPath, "", install.Facts{Queues: []gedata.Queue{queueOf(other)}})).To(BeEmpty())
		})

		It("counts an install whose pointer names the same file", func() {
			other := tree("other")
			shared := filepath.Join(root, "shared.yaml")
			Expect(config.WritePointer(other, shared)).To(Succeed())
			Expect(readerOf(shared, "", install.Facts{Queues: []gedata.Queue{queueOf(other)}})).NotTo(BeEmpty())
		})

		It("counts an install whose pointer it cannot trust, to be safe", func() {
			other := tree("other")
			Expect(config.WritePointer(other, filepath.Join(root, "elsewhere.yaml"))).To(Succeed())
			Expect(os.Chmod(filepath.Join(other, config.PointerRel), 0o666)).To(Succeed())
			Expect(readerOf(cellPath, "", install.Facts{Queues: []gedata.Queue{queueOf(other)}})).NotTo(BeEmpty())
		})

		It("skips the install being changed", func() {
			self := tree("self")
			Expect(readerOf(cellPath, self, install.Facts{Queues: []gedata.Queue{queueOf(self)}})).To(BeEmpty())
		})

		It("finds installs by their slurm-shim-env too, with GE's user@ prefix", func() {
			refs := install.ShimInstalls(install.Facts{
				PEs: []gedata.PE{{Name: "mpi", StartProcArgs: "root@/opt/b/bin/slurm-shim-env --pe"}},
			})
			Expect(refs).To(HaveLen(1))
			Expect(refs[0].Prefix).To(Equal("/opt/b"))
		})
	})

	Describe("retireMovedConfig", func() {
		It("renames the cell config when no other install reads it", func() {
			self := tree("self")
			var out bytes.Buffer
			retireMovedConfig(cellPath, self, install.Facts{Queues: []gedata.Queue{queueOf(self)}}, &out, &out)
			Expect(cellPath).NotTo(BeAnExistingFile())
			Expect(cellPath + movedSuffix).To(BeARegularFile())
		})

		It("keeps the cell config while another install still reads it", func() {
			self, other := tree("self"), tree("other")
			var out bytes.Buffer
			retireMovedConfig(cellPath, self, install.Facts{Queues: []gedata.Queue{queueOf(self), queueOf(other)}}, &out, &out)
			Expect(cellPath).To(BeARegularFile())
			Expect(out.String()).To(ContainSubstring("another slurm-shim install still reads it"))
		})

		It("leaves any other file in place and says so", func() {
			elsewhere := filepath.Join(root, "elsewhere.yaml")
			Expect(os.WriteFile(elsewhere, []byte("a: 1\n"), 0o644)).To(Succeed())
			var out bytes.Buffer
			retireMovedConfig(elsewhere, tree("self"), install.Facts{}, &out, &out)
			Expect(elsewhere).To(BeARegularFile())
			Expect(out.String()).To(ContainSubstring("left in place"))
		})

		It("is silent when there is nothing to retire", func() {
			var out bytes.Buffer
			retireMovedConfig(filepath.Join(root, "gone.yaml"), tree("self"), install.Facts{}, &out, &out)
			Expect(out.String()).To(BeEmpty())
		})
	})

	Describe("updatePointer", func() {
		It("writes the pointer for a config outside the cell, and removes it when the config comes back", func() {
			self := tree("self")
			shared := filepath.Join(root, "shared.yaml")
			var out bytes.Buffer
			Expect(updatePointer(self, configTarget{Path: shared, Source: sourceFlag, WritePointer: true}, &out, &out)).To(Equal(0))
			target, ok, err := config.ReadPointer(self)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(target).To(Equal(shared))

			Expect(updatePointer(self, configTarget{Path: cellPath, Source: sourceFlag}, &out, &out)).To(Equal(0))
			_, ok, _ = config.ReadPointer(self)
			Expect(ok).To(BeFalse())
		})

		It("leaves the pointer alone when $SLURM_SHIM_CONFIG chose the config", func() {
			self := tree("self")
			shared := filepath.Join(root, "shared.yaml")
			Expect(config.WritePointer(self, shared)).To(Succeed())
			var out bytes.Buffer
			Expect(updatePointer(self, configTarget{Path: "/tmp/oneoff.yaml", Source: config.SourceEnv}, &out, &out)).To(Equal(0))
			target, ok, _ := config.ReadPointer(self)
			Expect(ok).To(BeTrue())
			Expect(target).To(Equal(shared))
		})
	})

	Describe("cleanConfig", func() {
		deleteSlurmQ := install.Plan{Changes: []install.Change{{Kind: install.ChangeDeleteQueue, Object: "slurm.q"}}}
		write := func(p, s string) {
			Expect(os.MkdirAll(filepath.Dir(p), 0o755)).To(Succeed())
			Expect(os.WriteFile(p, []byte(s), 0o644)).To(Succeed())
		}

		It("keeps a purged config another install's pointer names", func() {
			other := tree("other")
			shared := filepath.Join(root, "shared.yaml")
			write(shared, "a: 1\n")
			Expect(config.WritePointer(other, shared)).To(Succeed())
			var out bytes.Buffer
			code := cleanConfig(shared, tree("self"), true, install.Plan{}, install.Facts{Queues: []gedata.Queue{queueOf(other)}}, &out, &out)
			Expect(code).To(Equal(0))
			Expect(shared).To(BeARegularFile())
			Expect(out.String()).To(ContainSubstring("kept"))
		})

		It("purges a config-dir config and its then-empty slurm-shim/, nothing else", func() {
			d := qontrolDir()
			cfg := filepath.Join(d, "slurm-shim", "config.yaml")
			write(cfg, "a: 1\n")
			var out bytes.Buffer
			Expect(cleanConfig(cfg, tree("self"), true, install.Plan{}, install.Facts{}, &out, &out)).To(Equal(0))
			Expect(filepath.Join(d, "slurm-shim")).NotTo(BeADirectory())
			Expect(filepath.Join(d, install.QontrolMarker)).To(BeARegularFile())
		})

		It("refuses a config that is not a regular file and leaves its target alone", func() {
			real := filepath.Join(root, "real.yaml")
			write(real, "a: 1\n")
			link := filepath.Join(root, "link.yaml")
			Expect(os.Symlink(real, link)).To(Succeed())
			var out bytes.Buffer
			Expect(cleanConfig(link, tree("self"), true, install.Plan{}, install.Facts{}, &out, &out)).To(Equal(1))
			Expect(real).To(BeARegularFile())
		})

		It("without purge, drops the partitions on a deleted queue and moves the default", func() {
			cfg := filepath.Join(root, "kept.yaml")
			write(cfg, "default_partition: slurm\npartitions:\n  slurm: {queue: slurm.q, pe: slurm-shim, slots: per-task}\n"+
				"  batch: {queue: all.q, pe: smp, slots: \"4\"}\n")
			var out bytes.Buffer
			Expect(cleanConfig(cfg, tree("self"), false, deleteSlurmQ, install.Facts{}, &out, &out)).To(Equal(0))
			data, err := os.ReadFile(cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).NotTo(ContainSubstring("slurm.q"))
			Expect(string(data)).To(ContainSubstring("default_partition: batch"))
		})
	})
})
