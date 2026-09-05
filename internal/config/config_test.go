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

var _ = Describe("allocation rule override and slot-rule validation", func() {
	It("defaults to probing the cluster", func() {
		Expect(config.Default().AllocationRuleOverride).To(Equal(config.OverrideAuto))
	})

	// A slot rule that cannot yield a positive count only WARNS at load. config.Load
	// is called by every command -- including slurm-shim-env, the PE start_proc_args
	// hook -- so one partition's typo must not take down squeue, sinfo, srun, or
	// environment fabrication for jobs that are already queued. computeSlots fails
	// the submission that actually names the partition.
	DescribeTable("warns about a slot rule that could never yield a positive count",
		func(rule string) {
			cfg, warns, err := config.Parse([]byte(
				"partitions:\n  batch: {queue: all.q, pe: make, slots: \"" + rule + "\"}\n"))
			Expect(err).NotTo(HaveOccurred(), "one bad partition must not fail every command")
			Expect(cfg).NotTo(BeNil())
			Expect(warns).To(ContainElement(SatisfyAll(
				ContainSubstring("batch"), ContainSubstring(rule))))
		},
		Entry("zero", "0"),
		Entry("negative", "-4"),
		Entry("not a number", "sixteen"),
	)

	It("names partitions in a stable order so warnings do not flap", func() {
		_, warns, err := config.Parse([]byte(
			"partitions:\n  zeta: {queue: a.q, pe: p, slots: \"0\"}\n" +
				"  alpha: {queue: a.q, pe: p, slots: \"-1\"}\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(warns).To(HaveLen(2))
		Expect(warns[0]).To(ContainSubstring("alpha"))
		Expect(warns[1]).To(ContainSubstring("zeta"))
	})

	DescribeTable("accepts the rules the translator understands",
		func(rule string) {
			_, _, err := config.Parse([]byte(
				"partitions:\n  batch: {queue: all.q, pe: make, slots: \"" + rule + "\"}\n"))
			Expect(err).NotTo(HaveOccurred())
		},
		Entry("per-task", "per-task"),
		Entry("a positive integer", "16"),
		Entry("empty (defaults to per-task)", ""),
	)

	// An unrecognized policy is forward compatibility, not a fatal error: a value
	// a newer shim understands must not stop this one from submitting.
	It("warns on an unknown global policy and falls back to auto", func() {
		cfg, warns, err := config.Parse([]byte("allocation_rule_override: sometimes\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(warns).To(ContainElement(ContainSubstring("sometimes")))
		Expect(cfg.AllocationRuleOverride).To(Equal(config.OverrideAuto))
	})

	It("warns on an unknown partition policy and clears it", func() {
		cfg, warns, err := config.Parse([]byte(
			"partitions:\n  batch: {queue: all.q, pe: make, allocation_rule_override: maybe}\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(warns).To(ContainElement(ContainSubstring("batch")))
		Expect(cfg.Partitions["batch"].AllocationRuleOverride).To(BeEmpty())
	})

	DescribeTable("resolves the effective policy partition-first",
		func(global, partition, want string) {
			cfg := config.Default()
			cfg.AllocationRuleOverride = global
			Expect(cfg.AllocationRuleMode(config.Partition{AllocationRuleOverride: partition})).To(Equal(want))
		},
		Entry("partition wins over global", config.OverrideAuto, config.OverrideNever, config.OverrideNever),
		Entry("partition wins the other way", config.OverrideNever, config.OverrideAlways, config.OverrideAlways),
		Entry("global applies when the partition is silent", config.OverrideNever, "", config.OverrideNever),
		Entry("auto when both are silent", "", "", config.OverrideAuto),
	)
})

var _ = Describe("control port validation", func() {
	parse := func(y string) (*config.Config, []string) {
		cfg, warns, err := config.Parse([]byte(y))
		Expect(err).NotTo(HaveOccurred())
		return cfg, warns
	}

	It("clamps a range that runs past 65535 so the printed firewall rule is valid", func() {
		cfg, warns := parse("control_port_base: 65000\ncontrol_port_range: 2000\n")
		Expect(cfg.ControlPortBase + cfg.ControlPortRange - 1).To(Equal(65535))
		Expect(warns).To(ContainElement(ContainSubstring("runs past 65535")))
	})

	It("warns when a base is set but the range makes it inert", func() {
		_, warns := parse("control_port_base: 63000\ncontrol_port_range: 0\n")
		Expect(warns).To(ContainElement(ContainSubstring("has no effect")))
	})

	It("warns about a privileged base srun cannot bind", func() {
		_, warns := parse("control_port_base: 80\ncontrol_port_range: 10\n")
		Expect(warns).To(ContainElement(ContainSubstring("privileged port")))
	})

	It("rejects a base outside the port space", func() {
		cfg, warns := parse("control_port_base: 70000\ncontrol_port_range: 10\n")
		Expect(cfg.ControlPortBase).To(Equal(0))
		Expect(warns).To(ContainElement(ContainSubstring("not a valid port")))
	})

	It("stays quiet for the shipped default and for the ephemeral opt-out", func() {
		_, warns := parse("control_port_base: 0\n")
		for _, w := range warns {
			Expect(w).NotTo(ContainSubstring("control_port"))
		}
		Expect(config.Default().ControlPortBase + config.Default().ControlPortRange - 1).
			To(BeNumerically("<=", 65535))
	})

	It("warns that the removed control_port key is ignored (migration signal)", func() {
		_, warns := parse("control_port: 30000\n")
		Expect(warns).To(ContainElement(ContainSubstring(`unknown config key "control_port"`)))
	})
})
