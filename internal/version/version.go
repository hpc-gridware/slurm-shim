// Package version renders the SLURM-parsable version string the shim reports.
//
// Frameworks feature-detect by regexing "sinfo -V" / "srun --version" output
// (REQ-DLV-003), so the string MUST read "slurm <compat> (slurm-shim <shim>)"
// with a real SLURM-shaped <compat> version ahead of the shim's own version.
package version

import "fmt"

// DefaultCompat is the SLURM compatibility version reported when config does
// not override it (spec section 9, default 24.05.0). The value a running shim
// reports comes from config; this constant is the built-in fallback.
const DefaultCompat = "24.05.0"

// Shim is the slurm-shim build version. It is overridden at build time via
// -ldflags "-X github.com/hpc-gridware/slurm-shim/internal/version.Shim=<v>";
// the default marks an untagged development build.
var Shim = "0.1.0-dev"

// String renders the version line for the given SLURM compatibility version,
// e.g. String("24.05.0") == "slurm 24.05.0 (slurm-shim 0.1.0-dev)".
func String(compat string) string {
	if compat == "" {
		compat = DefaultCompat
	}
	return fmt.Sprintf("slurm %s (slurm-shim %s)", compat, Shim)
}
