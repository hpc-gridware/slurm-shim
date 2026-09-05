// Package ports reports the TCP ranges a site must admit between cluster nodes,
// and prints ready-to-run firewall rules for them.
//
// It exists because the shim's two cross-node ports are invisible until a job
// hangs: the control channel steppers dial back on, and the rendezvous port the
// fabricated environment hands to frameworks like torchrun. On a network that
// filters between nodes (a GCP VPC, or firewalld) neither works until a rule
// admits it, and nothing else in the shim tells an admin that.
package ports

import (
	"fmt"
	"io"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// Run prints the port requirements and the rules that satisfy them, derived from
// this host's config so the output matches what the shim will actually bind.
func Run(cfg *config.Config, stdout io.Writer) int {
	control := rangeText(cfg.ControlPortBase, cfg.ControlPortRange)
	master := rangeText(cfg.MasterPortBase, cfg.MasterPortRange)

	fmt.Fprintln(stdout, "slurm-shim requires two TCP ranges between the nodes of a job.")
	fmt.Fprintln(stdout, "Both are inbound to the job's MASTER node, from the other nodes in")
	fmt.Fprintln(stdout, "the allocation -- no rule is needed in the other direction.")
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  control channel        TCP %-13s steppers dial back to srun\n", control)
	fmt.Fprintf(stdout, "  framework rendezvous   TCP %-13s MASTER_PORT for torchrun and friends\n", master)
	fmt.Fprintln(stdout)

	if cfg.ControlPortBase <= 0 || cfg.ControlPortRange <= 0 {
		fmt.Fprintln(stdout, "WARNING: control_port_base/control_port_range are disabled, so srun binds an")
		fmt.Fprintln(stdout, "ephemeral port that differs on every step. No firewall rule can describe it.")
		fmt.Fprintln(stdout, "Set control_port_base (e.g. 63000) unless this network is unfiltered.")
		fmt.Fprintln(stdout)
	}

	// Scope every rule to the cluster. The channel is token-authenticated, but a
	// predictable port reachable from anywhere is a needlessly larger target --
	// especially where the execd spool leaks the token to co-tenants (SI-51).
	fmt.Fprintln(stdout, "firewalld (run on every node; pick the zone your cluster network is in):")
	fmt.Fprintf(stdout, "  firewall-cmd --permanent --zone=internal --add-port=%s/tcp\n", portSpec(cfg.ControlPortBase, cfg.ControlPortRange))
	fmt.Fprintf(stdout, "  firewall-cmd --permanent --zone=internal --add-port=%s/tcp\n", portSpec(cfg.MasterPortBase, cfg.MasterPortRange))
	fmt.Fprintln(stdout, "  firewall-cmd --reload")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "GCP VPC (replace <network> and <tag>; keep it scoped to the cluster):")
	fmt.Fprintln(stdout, "  gcloud compute firewall-rules create slurm-shim-internal \\")
	fmt.Fprintln(stdout, "    --network=<network> \\")
	fmt.Fprintf(stdout, "    --allow=tcp:%s,tcp:%s \\\n", portSpec(cfg.ControlPortBase, cfg.ControlPortRange), portSpec(cfg.MasterPortBase, cfg.MasterPortRange))
	fmt.Fprintln(stdout, "    --source-tags=<tag> --target-tags=<tag>")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "nftables (inet filter, input chain; <cidr> is the cluster subnet):")
	fmt.Fprintf(stdout, "  nft add rule inet filter input ip saddr <cidr> tcp dport { %s, %s } accept\n",
		portSpec(cfg.ControlPortBase, cfg.ControlPortRange), portSpec(cfg.MasterPortBase, cfg.MasterPortRange))
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Scope the rules to the cluster's own subnet or tags, never 0.0.0.0/0: the")
	fmt.Fprintln(stdout, "control channel is token-authenticated, but the port should not be reachable")
	fmt.Fprintln(stdout, "from outside the cluster.")
	return 0
}

// rangeText renders a range for the human-readable summary.
func rangeText(base, count int) string {
	if base <= 0 || count <= 0 {
		return "ephemeral"
	}
	return fmt.Sprintf("%d-%d", base, base+count-1)
}

// portSpec renders a range in the "low-high" form every firewall CLI accepts.
func portSpec(base, count int) string {
	if base <= 0 || count <= 0 {
		return "<unset>"
	}
	return fmt.Sprintf("%d-%d", base, base+count-1)
}
