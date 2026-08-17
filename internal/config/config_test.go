package config_test

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

var _ = Describe("Config loading", func() {
	Describe("defaults [REQ-CFG-001]", func() {
		It("returns built-in defaults when no file is present", func() {
			GinkgoT().Setenv(config.EnvVar, filepath.Join(GinkgoT().TempDir(), "absent.yaml"))
			cfg, warns, err := config.Load()
			Expect(err).NotTo(HaveOccurred())
			Expect(warns).To(BeEmpty())
			Expect(cfg.CompatVersion).To(Equal("24.05.0"))
			Expect(cfg.Launcher).To(Equal("qrsh-inherit"))
			Expect(cfg.KillOnBadExit).To(BeTrue())
			Expect(cfg.KillWait.Duration).To(Equal(30 * time.Second))
			Expect(cfg.Standalone).To(Equal("reject"))
			Expect(cfg.GPU.Discovery).To(Equal("qstat-gres"))
		})

		It("reads the file named by the config env var", func() {
			path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
			Expect(os.WriteFile(path, []byte("launcher: ssh\n"), 0o600)).To(Succeed())
			GinkgoT().Setenv(config.EnvVar, path)
			cfg, _, err := config.Load()
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Launcher).To(Equal("ssh"))
		})
	})

	Describe("overlay onto defaults", func() {
		It("overrides only the keys present and keeps other defaults", func() {
			cfg, warns, err := config.Parse([]byte("kill_on_bad_exit: false\nlaunch_ramp: 8\n"))
			Expect(err).NotTo(HaveOccurred())
			Expect(warns).To(BeEmpty())
			Expect(cfg.KillOnBadExit).To(BeFalse())
			Expect(cfg.LaunchRamp).To(Equal(8))
			// Untouched keys keep their defaults.
			Expect(cfg.CompatVersion).To(Equal("24.05.0"))
			Expect(cfg.LaunchTimeout.Duration).To(Equal(60 * time.Second))
		})

		It("parses durations with time.ParseDuration syntax [REQ-CFG-002]", func() {
			cfg, _, err := config.Parse([]byte("kill_wait: 45s\nping_deadline: 90s\norphan_grace: 5m\n"))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.KillWait.Duration).To(Equal(45 * time.Second))
			Expect(cfg.PingDeadline.Duration).To(Equal(90 * time.Second))
			Expect(cfg.OrphanGrace.Duration).To(Equal(5 * time.Minute))
		})

		It("parses nested partition, pe, and gpu maps", func() {
			doc := `
partitions:
  gpu: {queue: gpu.q, pe: gpu.pe, slots: "8"}
pes:
  gpu.pe: {task_policy: gpu}
partition_aliases: {gpu.q: gpu}
gpu:
  isolation: cgroup
`
			cfg, _, err := config.Parse([]byte(doc))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Partitions["gpu"].PE).To(Equal("gpu.pe"))
			Expect(cfg.Partitions["gpu"].Slots).To(Equal("8"))
			Expect(cfg.PEs["gpu.pe"].TaskPolicy).To(Equal("gpu"))
			Expect(cfg.PartitionAliases["gpu.q"]).To(Equal("gpu"))
			Expect(cfg.GPU.Isolation).To(Equal("cgroup"))
			// gpu.discovery not overridden, keeps default.
			Expect(cfg.GPU.Discovery).To(Equal("qstat-gres"))
		})
	})

	Describe("forward compatibility and errors [REQ-CFG-002]", func() {
		It("warns on unknown keys and continues", func() {
			cfg, warns, err := config.Parse([]byte("launcher: ssh\nfuture_key: 1\n"))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Launcher).To(Equal("ssh"))
			Expect(warns).To(ContainElement(ContainSubstring("future_key")))
		})

		It("hard-errors on a malformed duration", func() {
			_, _, err := config.Parse([]byte("kill_wait: 3weeks\n"))
			Expect(err).To(HaveOccurred())
		})

		It("hard-errors on malformed YAML", func() {
			_, _, err := config.Parse([]byte("launcher: [unclosed\n"))
			Expect(err).To(HaveOccurred())
		})
	})
})
