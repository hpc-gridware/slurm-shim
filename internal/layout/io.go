package layout

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// StateDirFor returns the per-job state directory under tmpdir, refusing an
// empty tmpdir rather than falling back to a shared /tmp path.
//
// Everything in that directory is allocation truth -- layout.json and the step
// counter -- and the fallback would read it from /tmp/slurm_shim, a world-known
// path any local user can create and populate. The write side refuses the same
// case (fabricator.EnsureStateDir), so this keeps both ends of the contract
// aligned. Under Grid Engine the queue's tmpdir gives every batch and PE job a
// TMPDIR, so an empty value means this is not running inside a job at all --
// which is what callers report.
func StateDirFor(tmpdir string) (string, error) {
	if tmpdir == "" {
		return "", fmt.Errorf("TMPDIR is not set, so there is no per-job state: %w", os.ErrNotExist)
	}
	return filepath.Join(tmpdir, StateDir), nil
}

// ErrSchemaVersion reports a layout file whose schema_version this build does
// not understand. Callers map it to exit code 7 (REQ-LAY-005).
type ErrSchemaVersion struct{ Got, Want int }

func (e ErrSchemaVersion) Error() string {
	return fmt.Sprintf("unsupported layout schema_version %d (this build understands %d)", e.Got, e.Want)
}

// Write serializes the layout to <dir>/layout.json atomically: it writes a
// temp file in the same directory, fsyncs it, sets mode 0600, and renames it
// into place, so a reader never observes a partial file (REQ-LAY-004). The
// directory is created 0700 if absent.
func Write(dir string, l *Layout) (err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, LayoutFile+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Clean up the temp file on any failure before the rename succeeds.
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, LayoutFile))
}

// Read loads and validates a layout file. It rejects an unknown schema_version
// with ErrSchemaVersion (REQ-LAY-005).
func Read(path string) (*Layout, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Layout
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("parsing layout %s: %w", path, err)
	}
	if l.SchemaVersion != SchemaVersion {
		return nil, ErrSchemaVersion{Got: l.SchemaVersion, Want: SchemaVersion}
	}
	return &l, nil
}
