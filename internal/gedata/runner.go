// Package gedata is the boundary to the Grid Engine client tools. It is one of
// the two packages permitted to call os/exec (REQ-IMP-001); every other package
// reaches GE only through the Runner interface, so the whole shim is testable
// with a fake that replays recorded fixtures.
package gedata

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner abstracts external process execution (GE clients: qstat, qsub, qdel,
// qhost, qacct, qmod, getent). A non-zero command exit is reported through the
// exit return, not err; err is reserved for spawn/context failures (binary
// missing, context cancelled or timed out) so callers can distinguish "the tool
// ran and said no" from "the tool could not run".
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr []byte, exit int, err error)
}

// ExecRunner is the production Runner backed by os/exec.
type ExecRunner struct{}

// Run executes name with args, capturing stdout and stderr separately.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, ResolveCommand(name), args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out.Bytes(), errBuf.Bytes(), 0, nil
	case errors.As(err, &exitErr):
		// The tool ran and exited non-zero: report the code, not an error.
		return out.Bytes(), errBuf.Bytes(), exitErr.ExitCode(), nil
	default:
		// Spawn or context failure: the command did not run to completion.
		return out.Bytes(), errBuf.Bytes(), -1, err
	}
}

// ResolveCommand locates a GE client tool (qstat, qrsh, ...) when the caller
// runs in a non-login shell that never sourced the site profile: GE runs batch
// job scripts and PE start_proc_args hooks that way, so PATH lacks
// $SGE_ROOT/bin/$ARC. A bare name already on PATH, or any name containing a path
// separator, is used unchanged; otherwise it falls back to
// $SGE_ROOT/bin/$ARC/<name>: GE exports SGE_ROOT and the arch (as ARC, or
// SGE_ARCH on some installs) into the job environment. System tools such as
// getent stay on PATH and are unaffected. If nothing resolves, the bare name is
// returned so exec fails with its usual "not found" message. Shared by the
// Runner and the qrsh launcher.
func ResolveCommand(name string) string {
	if strings.ContainsRune(name, filepath.Separator) {
		return name
	}
	if _, err := exec.LookPath(name); err == nil {
		return name
	}
	root := os.Getenv("SGE_ROOT")
	arch := os.Getenv("ARC")
	if arch == "" {
		arch = os.Getenv("SGE_ARCH")
	}
	if root != "" && arch != "" {
		cand := filepath.Join(root, "bin", arch, name)
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand
		}
	}
	return name
}
