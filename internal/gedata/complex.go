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

// Numeric reports whether a request can carry a count (INT, DOUBLE, MEMORY).
func (c Complex) Numeric() bool {
	switch strings.ToUpper(c.Type) {
	case "INT", "DOUBLE", "MEMORY":
		return true
	}
	return false
}

// CanRequest reports whether a job may name the complex in -l.
func (c Complex) CanRequest() bool {
	r := strings.ToUpper(c.Requestable)
	return r == "YES" || r == "FORCED"
}

// Catalog is the cluster's complex table.
type Catalog []Complex

// Lookup finds a complex by its name or shortcut, either of which a job may
// request.
func (cat Catalog) Lookup(name string) (Complex, bool) {
	for _, c := range cat {
		if c.Name == name || (c.Shortcut != "" && c.Shortcut == name) {
			return c, true
		}
	}
	return Complex{}, false
}

// Complexes reads the cluster's complex table, upper-casing Type, Relop,
// Requestable and Consumable as Grid Engine prints them (BOOL, EXCL, JOB, ...).
//
// The `qconf -sc` table is the same eight-column layout go-clusterscheduler's
// ShowAllComplexes parses; that library couples parsing to its own exec, so the
// few lines are mirrored here to keep the call on the shim's Runner (testable,
// timeout-controlled). Exposing a pure parser upstream would let this delegate.
func Complexes(ctx context.Context, r Runner) (Catalog, error) {
	if r == nil {
		return nil, fmt.Errorf("gedata: no runner")
	}
	out, err := runChecked(ctx, r, "qconf", "-sc")
	if err != nil {
		return nil, err
	}
	var cat Catalog
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 8 {
			continue
		}
		cat = append(cat, Complex{
			Name: f[0], Shortcut: f[1], Type: strings.ToUpper(f[2]), Relop: strings.ToUpper(f[3]),
			Requestable: strings.ToUpper(f[4]), Consumable: strings.ToUpper(f[5]),
		})
	}
	return cat, nil
}

// ConsumableScope reports how this cluster interprets a request for the named
// complex, as one of the Consumable* constants.
//
// It matters because the same `-l mem_free=4G` means "4G per slot" under
// consumable YES and "4G on this host" under NO, so a per-node figure derived
// from it is only correct if the scope is known. Verified on OCS 9.1.5: every
// stock memory complex, mem_free included, is `consumable NO`.
func ConsumableScope(ctx context.Context, r Runner, name string) (string, error) {
	if r == nil || name == "" {
		return "", fmt.Errorf("gedata: no runner or complex name")
	}
	cat, err := Complexes(ctx, r)
	if err != nil {
		return "", err
	}
	if c, ok := cat.Lookup(name); ok {
		return c.Consumable, nil
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
