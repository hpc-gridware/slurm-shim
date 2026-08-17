package fabricator

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// File names written under the state directory (spec section 11.1).
const (
	EnvFile      = "environment"
	SentinelFile = "environment.failed"
)

// StateDir returns the per-job state directory under TMPDIR.
func StateDir(tmpdir string) string {
	return filepath.Join(tmpdir, layout.StateDir)
}

// RenderExports produces the shell lines for the environment file or the
// --export stdout: the unset preamble (REQ-ENV-011) followed by shell-quoted
// exports (REQ-ENV-002). A disabled (scrub-only) result renders only the unset
// preamble.
func (r *Result) RenderExports() string {
	var b strings.Builder
	for _, k := range r.Unset {
		b.WriteString("unset " + k + "\n")
	}
	if r.Disabled {
		return b.String()
	}
	for _, kv := range r.Exports {
		b.WriteString("export " + kv.Key + "=" + shellQuote(kv.Value) + "\n")
	}
	return b.String()
}

// WriteState writes the layout file and the environment file into dir, both
// mode 0600 (REQ-LAY-004, REQ-FAB-005, SI-45). A scrub-only result has no
// layout and writes only the environment file.
func WriteState(dir string, r *Result) error {
	if r.Layout != nil {
		if err := layout.Write(dir, r.Layout); err != nil {
			return err
		}
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFile0600(filepath.Join(dir, EnvFile), []byte(r.RenderExports()))
}

// WriteSentinel writes the environment.failed sentinel (REQ-FAB-009). The
// fabricator writes it and exits 0 so a failed start_proc_args cannot error the
// queue; the sourcing hook enforces the failure.
func WriteSentinel(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFile0600(filepath.Join(dir, SentinelFile), []byte("fabrication failed\n"))
}

// writeFile0600 writes data and forces mode 0600 regardless of umask.
func writeFile0600(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
