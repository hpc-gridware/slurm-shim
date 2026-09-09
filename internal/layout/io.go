package layout

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	if MinReadableSchemaVersion < SchemaVersion {
		return fmt.Sprintf("unsupported layout schema_version %d (this build reads %d-%d)",
			e.Got, MinReadableSchemaVersion, SchemaVersion)
	}
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

// Read loads and validates a layout file. It accepts the current schema and any
// older one it can migrate, and rejects anything else with ErrSchemaVersion
// (REQ-LAY-005).
//
// A v1 layout is migrated in memory: v1 wrote GPU device ids as JSON numbers and
// v2 writes them as strings, so upgrading the shim binary while a job is running
// must not strand that job's in-flight allocation. Only a downgrade fails, which
// was already true before v2.
func Read(path string) (*Layout, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parsing layout %s: %w", path, err)
	}
	if probe.SchemaVersion < MinReadableSchemaVersion || probe.SchemaVersion > SchemaVersion {
		return nil, ErrSchemaVersion{Got: probe.SchemaVersion, Want: SchemaVersion}
	}
	if probe.SchemaVersion < SchemaVersion {
		return migrate(path, data, probe.SchemaVersion)
	}
	var l Layout
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("parsing layout %s: %w", path, err)
	}
	return &l, nil
}

// migrate reads an older layout into the current shape.
//
// v1 differs from v2 only in the JSON type of the GPU device ids: numbers then,
// strings now. Rather than keeping a second copy of the whole Layout struct in
// sync forever, the document itself is rewritten -- device id arrays converted
// in place -- and then decoded normally. A layout is read once per step, so the
// extra pass costs nothing, and this cannot rot when Layout gains a field.
//
// Every shape it does not recognise is an error, never a quiet skip. The map
// pass buys flexibility at the cost of the strictness a typed decode gives for
// free, and a migration that silently produced a GPU job with no device masks
// would be the exact silent-wrong-device failure the string ids exist to stop.
func migrate(path string, data []byte, from int) (*Layout, error) {
	if from != 1 {
		return nil, ErrSchemaVersion{Got: from, Want: SchemaVersion}
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing v1 layout %s: %w", path, err)
	}
	if err := migrateGPUArrays(doc); err != nil {
		return nil, fmt.Errorf("migrating v1 layout %s: %w", path, err)
	}
	doc["schema_version"] = SchemaVersion

	fixed, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("migrating v1 layout %s: %w", path, err)
	}
	var l Layout
	if err := json.Unmarshal(fixed, &l); err != nil {
		return nil, fmt.Errorf("parsing migrated layout %s: %w", path, err)
	}
	return &l, nil
}

// migrateGPUArrays converts every device id array in the document. Absent keys
// are fine -- a CPU-only layout has no "gpus" anywhere -- but a key present with
// an unexpected shape is not.
func migrateGPUArrays(doc map[string]any) error {
	if v, present := doc["nodes"]; present && v != nil {
		nodes, ok := v.([]any)
		if !ok {
			return fmt.Errorf("nodes: expected an array, got %T", v)
		}
		for i, n := range nodes {
			if err := stringifyGPUs(n); err != nil {
				return fmt.Errorf("nodes[%d]: %w", i, err)
			}
		}
	}
	v, present := doc["tasks"]
	if !present || v == nil {
		return nil
	}
	tasks, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("tasks: expected an object, got %T", v)
	}
	rv, present := tasks["rank_map"]
	if !present || rv == nil {
		return nil
	}
	ranks, ok := rv.([]any)
	if !ok {
		return fmt.Errorf("tasks.rank_map: expected an array, got %T", rv)
	}
	for i, r := range ranks {
		if err := stringifyGPUs(r); err != nil {
			return fmt.Errorf("tasks.rank_map[%d]: %w", i, err)
		}
	}
	return nil
}

// stringifyGPUs rewrites one object's "gpus" array from JSON numbers to strings.
// A value already a string is left alone, so the pass is idempotent. An absent or
// null array is fine; anything else present is an error rather than a guess.
func stringifyGPUs(obj any) error {
	m, ok := obj.(map[string]any)
	if !ok {
		return fmt.Errorf("expected an object, got %T", obj)
	}
	v, present := m["gpus"]
	if !present || v == nil {
		return nil
	}
	ids, ok := v.([]any)
	if !ok {
		return fmt.Errorf("gpus: expected an array, got %T", v)
	}
	out := make([]any, len(ids))
	for i, id := range ids {
		switch t := id.(type) {
		case float64:
			// v1 wrote integer device indices; a fraction is not one.
			if t != float64(int64(t)) {
				return fmt.Errorf("gpus[%d]: %v is not an integer device id", i, t)
			}
			out[i] = strconv.FormatInt(int64(t), 10)
		case string:
			out[i] = t
		default:
			return fmt.Errorf("gpus[%d]: expected a number or string, got %T", i, id)
		}
	}
	m["gpus"] = out
	return nil
}
