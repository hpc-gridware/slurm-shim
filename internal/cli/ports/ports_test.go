package ports_test

import (
	"bytes"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/cli/ports"
	"github.com/hpc-gridware/slurm-shim/internal/config"
)

func TestPorts(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Ports Suite")
}

var _ = Describe("slurm-shim ports", func() {
	report := func(cfg *config.Config) string {
		var b bytes.Buffer
		Expect(ports.Run(cfg, &b)).To(Equal(0))
		return b.String()
	}

	It("prints both ranges derived from the site's config, not hardcoded", func() {
		out := report(&config.Config{
			ControlPortBase: 51000, ControlPortRange: 100,
			MasterPortBase: 41000, MasterPortRange: 500,
		})
		Expect(out).To(ContainSubstring("51000-51099"))
		Expect(out).To(ContainSubstring("41000-41499"))
		Expect(out).NotTo(ContainSubstring("63000"), "must not print the default when overridden")
	})

	It("emits rules for firewalld, GCP and nftables", func() {
		out := report(config.Default())
		Expect(out).To(ContainSubstring("firewall-cmd --permanent"))
		Expect(out).To(ContainSubstring("gcloud compute firewall-rules create"))
		Expect(out).To(ContainSubstring("nft add rule"))
	})

	It("states the direction, which is what a rule needs", func() {
		Expect(report(config.Default())).To(ContainSubstring("MASTER node"))
	})

	It("insists the rules stay scoped to the cluster", func() {
		Expect(report(config.Default())).To(ContainSubstring("never 0.0.0.0/0"))
	})

	It("warns when the control range is disabled, since no rule can then work", func() {
		out := report(&config.Config{
			ControlPortBase: 0, ControlPortRange: 0,
			MasterPortBase: 20000, MasterPortRange: 10000,
		})
		Expect(out).To(ContainSubstring("ephemeral"))
		Expect(out).To(ContainSubstring("No firewall rule can describe it"))
	})

	It("ships a default control range above the usual ephemeral ceiling (60999)", func() {
		Expect(config.Default().ControlPortBase).To(BeNumerically(">", 60999))
	})
})
