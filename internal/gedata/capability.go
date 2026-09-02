package gedata

import (
	"context"
	"strings"
	"sync"
)

// Capabilities answers capability questions about the local Grid Engine clients,
// probing at most once per instance.
//
// It is caller-owned on purpose. A package-level cache would buy nothing -- each
// shim command is a one-shot process that submits once, so a process-lifetime
// answer can never be reused -- and under Ginkgo, where one binary runs every
// spec in a package, it would leak the first spec's answer into all the others,
// making the supported and unsupported paths impossible to cover in one run.
type Capabilities struct {
	runner Runner
	once   sync.Once
	par    bool
	parErr error
}

// NewCapabilities returns a prober backed by runner.
func NewCapabilities(runner Runner) *Capabilities {
	return &Capabilities{runner: runner}
}

// AllocationRuleOverride reports whether this cluster's qsub understands `-par`
// (OCS 9.1.5 and newer; absent in classic SGE and OCS 9.0.x).
//
// The three results are distinct on purpose: (true, nil) supported, (false, nil)
// the client ran and has no -par, and (false, err) the probe itself could not
// run. Collapsing the last two would tell someone with no GE client on their PATH
// that their cluster is too old, which is both wrong and unactionable.
//
// It probes `qsub -help`, which is client-side usage text: it exits 0 and prints
// the same output with the qmaster unreachable, so the answer never depends on
// cluster state. Feature detection rather than a version comparison, because the
// same client ships under two product names (OCS and Gridware Cluster Scheduler)
// with independent version strings.
func (c *Capabilities) AllocationRuleOverride(ctx context.Context) (bool, error) {
	c.once.Do(func() {
		out, errOut, _, err := c.runner.Run(ctx, "qsub", "-help")
		if err != nil {
			c.parErr = err
			return
		}
		// Usage text goes to stdout here, but a repackaged client could send it to
		// stderr; scanning both costs nothing and avoids a silent false negative.
		c.par = hasAllocationRuleOption(out) || hasAllocationRuleOption(errOut)
	})
	return c.par, c.parErr
}

// hasAllocationRuleOption looks for qsub's own usage entry for -par:
//
//	[-par allocation_rule]                   set the parallel job allocation rule
//
// Matching the bracketed usage form rather than a bare "-par" substring keeps
// prose that merely mentions the option -- a banner line, a fixture header, a
// translated description -- from reading as support.
func hasAllocationRuleOption(usage []byte) bool {
	for _, line := range strings.Split(string(usage), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "[-par ") || t == "[-par]" {
			return true
		}
	}
	return false
}
