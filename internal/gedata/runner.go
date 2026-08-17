// Package gedata is the boundary to the Grid Engine client tools. It is one of
// the two packages permitted to call os/exec (REQ-IMP-001); every other package
// reaches GE only through the Runner interface, so the whole shim is testable
// with a fake that replays recorded fixtures.
package gedata

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
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
	cmd := exec.CommandContext(ctx, name, args...)
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
