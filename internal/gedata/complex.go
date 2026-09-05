package gedata

import (
	"context"
	"fmt"
	"strings"
)

// Consumable scopes as Grid Engine reports them in `qconf -sc`. They decide how
// a `-l <complex>=<value>` request is interpreted, and therefore whether the
// value must be multiplied by the node's slot count to get a per-node figure.
const (
	ConsumablePerSlot = "YES"  // value is per slot: GE debits value x slots
	ConsumablePerJob  = "JOB"  // value is for the whole job
	ConsumablePerHost = "HOST" // value is per host
	ConsumableNone    = "NO"   // not consumable: a host filter, never a grant
)

// ConsumableScope reports how this cluster interprets a request for the named
// complex, as one of the Consumable* constants.
//
// It matters because the same `-l mem_free=4G` means "4G per slot" under
// consumable YES and "4G on this host" under NO, so a per-node figure derived
// from it is only correct if the scope is known. Verified on OCS 9.1.5: every
// stock memory complex, mem_free included, is `consumable NO`.
//
// The `qconf -sc` table is the same eight-column layout go-clusterscheduler's
// ShowAllComplexes parses; that library couples parsing to its own exec, so the
// few lines are mirrored here to keep the call on the shim's Runner (testable,
// timeout-controlled). Exposing a pure parser upstream would let this delegate.
func ConsumableScope(ctx context.Context, r Runner, name string) (string, error) {
	if r == nil || name == "" {
		return "", fmt.Errorf("gedata: no runner or complex name")
	}
	out, _, exit, err := r.Run(ctx, "qconf", "-sc")
	if err != nil {
		return "", fmt.Errorf("qconf -sc: %w", err)
	}
	if exit != 0 {
		return "", fmt.Errorf("qconf -sc: exit %d", exit)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 8 {
			continue
		}
		// Column 0 is the name, column 1 its shortcut; either may be requested.
		if f[0] == name || f[1] == name {
			return strings.ToUpper(f[5]), nil
		}
	}
	return "", fmt.Errorf("gedata: complex %q not found in qconf -sc", name)
}

// PerNodeMultiplier is the factor turning a requested complex value into the
// per-node amount for the given scope. Only a per-slot consumable multiplies.
func PerNodeMultiplier(scope string, slots int) int {
	if strings.EqualFold(scope, ConsumablePerSlot) && slots > 0 {
		return slots
	}
	return 1
}
