package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// StateRel is where install records what it did, relative to the prefix.
const StateRel = "etc/install-state.json"

// stateSchemaVersion is bumped when State changes incompatibly; ReadState
// refuses a record from a newer installer rather than misread it.
const stateSchemaVersion = 1

// State is what install did to the cluster, so uninstall can undo exactly
// that: what it created, the PEs it added to queues, and the old value of
// anything it replaced with --force (the only way such a change can be
// restored). Uninstall acts as root on it, so ReadState trusts only a record
// that root (or the user running it) owns and nobody else can write, and
// uninstall still re-checks every object against the cluster before it
// deletes or restores anything.
type State struct {
	SchemaVersion  int         `json:"schema_version"`
	Prefix         string      `json:"prefix"`
	CreatedQueue   string      `json:"created_queue,omitempty"`
	CreatedPE      string      `json:"created_pe,omitempty"`
	CreatedComplex string      `json:"created_complex,omitempty"`
	PEListAdds     []PEListAdd `json:"pe_list_adds,omitempty"`
	Replaced       []Replaced  `json:"replaced,omitempty"`
	// Config is the config file install wrote, for uninstall to name; it is
	// never a path uninstall deletes on the record's word.
	Config string `json:"config,omitempty"`
}

// PEListAdd is one PE install added to a queue's pe_list.
type PEListAdd struct {
	Queue string `json:"queue"`
	PE    string `json:"pe"`
}

// Replaced is one attribute install overwrote: Object is "queue <name>" or
// "pe <name>", Old the value before.
type Replaced struct {
	Object string `json:"object"`
	Attr   string `json:"attr"`
	Old    string `json:"old"`
}

// ReadState reads prefix's install state; nil without an error when there is
// none (an install made before install recorded its state). A record that is
// a symlink, not a regular file, writable by group or others, or owned by
// someone other than root or the caller is refused: its values are written
// back into the cluster as a manager.
func ReadState(prefix string) (*State, error) {
	path := filepath.Join(prefix, StateRel)
	for _, p := range []string{filepath.Dir(path), path} {
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if err := trusted(p, fi); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.SchemaVersion > stateSchemaVersion {
		return nil, fmt.Errorf("%s: schema_version %d is newer than this binary reads (%d); use the slurm-shim that wrote it",
			path, s.SchemaVersion, stateSchemaVersion)
	}
	return &s, nil
}

// trusted checks one path of the record: a real directory or regular file
// (never a symlink), not writable by group or others, owned by root or by the
// user running the command.
func trusted(path string, fi os.FileInfo) error {
	mode := fi.Mode()
	if mode&os.ModeSymlink != 0 || (!mode.IsRegular() && !mode.IsDir()) {
		return fmt.Errorf("%s: not a regular file or directory; refusing to trust the install record", path)
	}
	if mode.Perm()&0o022 != 0 {
		return fmt.Errorf("%s: writable by group or others (%v); refusing to trust the install record", path, mode.Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s: owned by uid %d, neither root nor you; refusing to trust the install record", path, st.Uid)
	}
	return nil
}

// WriteState writes s under prefix (0644: it holds no secret, and doctor may
// read it). It writes beside and renames over, so a planted symlink is
// replaced rather than followed and a crash never leaves half a record.
func WriteState(prefix string, s *State) error {
	s.SchemaVersion = stateSchemaVersion
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(prefix, StateRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".install-state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Intend records the objects the plan is about to create, before it is
// applied: an install interrupted between creating them and recording the
// result then still has a record naming them, so a re-run finishes the job
// and uninstall can remove them. Uninstall checks each one exists and is
// still the shim's before acting on it.
func (s *State) Intend(p Plan) {
	for _, c := range p.Changes {
		switch c.Kind {
		case ChangeAddQueue:
			s.CreatedQueue = c.Object
		case ChangeAddPE:
			s.CreatedPE = c.Object
		case ChangeAddComplex:
			s.CreatedComplex = c.Object
		}
	}
}

// Record folds what an apply did into s: the changes that succeeded. A re-run
// keeps the first recorded old value of an attribute, which is the site's,
// never one install wrote itself.
func (s *State) Record(r Report) {
	for _, o := range r.Outcomes {
		if o.Err != nil {
			continue
		}
		c := o.Change
		switch c.Kind {
		case ChangeAddQueue, ChangeAddPE, ChangeAddComplex:
			s.Intend(Plan{Changes: []Change{c}})
		case ChangeAddToPEList:
			add := PEListAdd{Queue: c.Object, PE: c.New}
			if !slices.Contains(s.PEListAdds, add) {
				s.PEListAdds = append(s.PEListAdds, add)
			}
		case ChangeSetStarter:
			if c.Old != "" {
				s.replace("queue "+c.Object, "starter_method", c.Old)
			}
		case ChangeSetPEAttr:
			if c.Old != "" {
				s.replace("pe "+c.Object, c.Attr, c.Old)
			}
		}
	}
}

func (s *State) replace(object, attr, old string) {
	if _, ok := s.replaced(object, attr); !ok {
		s.Replaced = append(s.Replaced, Replaced{Object: object, Attr: attr, Old: old})
	}
}

// replaced returns the recorded old value of object's attr.
func (s *State) replaced(object, attr string) (string, bool) {
	if s == nil {
		return "", false
	}
	for _, r := range s.Replaced {
		if r.Object == object && r.Attr == attr {
			return r.Old, true
		}
	}
	return "", false
}

// replacedAttrs returns every recorded old value for object, in record order.
func (s *State) replacedAttrs(object string) []Replaced {
	if s == nil {
		return nil
	}
	var out []Replaced
	for _, r := range s.Replaced {
		if r.Object == object {
			out = append(out, r)
		}
	}
	return out
}
