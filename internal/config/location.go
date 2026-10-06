package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// PointerRel is the config location pointer, relative to the install prefix:
// one line naming the config file every command of that install loads. The
// installer writes it when the config is not at the cell path, so a config
// outside $SGE_ROOT (a Qontrol config dir) is found on every host without an
// environment variable. It lives in the install's own root-owned tree: moving
// the config stays an installer act even where editing the file is delegated.
const PointerRel = "etc/config-path"

// maxPointerBytes bounds a pointer read: it holds one path.
const maxPointerBytes = 4096

// Where a config came from, as `slurm-shim config path` and doctor name it.
const (
	SourceEnv      = "env"
	SourcePointer  = "pointer"
	SourceCell     = "cell"
	SourceEtc      = "etc"
	SourceDefaults = "defaults"
)

// Location is the config a command uses and why.
type Location struct {
	// Path is the file; "" for SourceDefaults, or the $SLURM_SHIM_CONFIG that
	// names no file.
	Path   string
	Source string
	// Exists reports whether Path is a file to load; false means the
	// compiled-in defaults apply.
	Exists bool
	// Pointer is the install's pointer file when there is one, also when
	// $SLURM_SHIM_CONFIG overrides it.
	Pointer string
}

// SelfPrefix is the install tree this binary runs from: every command is a
// symlink to <prefix>/bin/slurm-shim, which os.Executable resolves. "" when it
// cannot be determined.
func SelfPrefix() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Dir(filepath.Dir(exe))
}

// Resolve finds the config for the install at prefix ("" for no install
// tree): $SLURM_SHIM_CONFIG alone when set; else the pointer's target alone
// when the install has a pointer; else the first existing of the cell path and
// DefaultPath; else the defaults.
//
// Only a file that does not exist falls through. A pointer that is untrusted,
// malformed, or names a file this user cannot read is an error, never a
// fall-through to the cell path or the defaults: the admin chose a location,
// and a job silently running on another config is far harder to find than one
// that fails naming the pointer. Likewise a cell or $SLURM_SHIM_CONFIG path
// that cannot be checked (a directory this user cannot search) is an error.
func Resolve(prefix string) (Location, error) {
	var loc Location
	var pointerErr error
	if prefix != "" {
		p := filepath.Join(prefix, PointerRel)
		if _, err := os.Lstat(p); err == nil {
			loc.Pointer = p
		} else if !errors.Is(err, fs.ErrNotExist) {
			pointerErr = fmt.Errorf("config pointer %s: %w", p, err)
		}
	}
	if p := os.Getenv(EnvVar); p != "" {
		ok, err := present(p)
		if err != nil {
			return loc, fmt.Errorf("%s=%s: %w", EnvVar, p, err)
		}
		loc.Path, loc.Source, loc.Exists = p, SourceEnv, ok
		if !ok {
			loc.Source = SourceDefaults
		}
		return loc, nil
	}
	if pointerErr != nil {
		return loc, pointerErr
	}
	if loc.Pointer != "" {
		target, _, err := ReadPointer(prefix)
		if err != nil {
			return loc, err
		}
		if err := readable(target); err != nil {
			return loc, fmt.Errorf("config pointer %s names %s, which cannot be read: %w (restore the file, "+
				"or re-run slurm-shim install --apply with --config PATH or --config-dir DIR)", loc.Pointer, target, err)
		}
		loc.Path, loc.Source, loc.Exists = target, SourcePointer, true
		return loc, nil
	}
	for _, p := range fallbackPaths() {
		ok, err := present(p)
		if err != nil {
			return loc, fmt.Errorf("config %s: %w", p, err)
		}
		if ok {
			loc.Path, loc.Exists = p, true
			loc.Source = SourceCell
			if p == DefaultPath {
				loc.Source = SourceEtc
			}
			return loc, nil
		}
	}
	loc.Source = SourceDefaults
	return loc, nil
}

// InstallPath is the config an install or uninstall of the tree at prefix
// acts on by default: $SLURM_SHIM_CONFIG, else InstallDefault.
func InstallPath(prefix string) (path, source string, err error) {
	if p := os.Getenv(EnvVar); p != "" {
		return p, SourceEnv, nil
	}
	return InstallDefault(prefix)
}

// InstallDefault is the config every job of the install at prefix reads, as
// far as the install decides it: the pointer's target (which need not exist
// yet: install re-creates it), else CellPath. The pointer is read from prefix,
// not from the running binary's tree, because install may run from an
// unpacked payload elsewhere.
func InstallDefault(prefix string) (path, source string, err error) {
	target, ok, err := ReadPointer(prefix)
	if err != nil {
		return "", "", err
	}
	if ok {
		return target, SourcePointer, nil
	}
	return CellPath(), SourceCell, nil
}

// ReadPointer reads the install's config pointer; ok is false when there is
// none. The pointer decides which file every job loads, so it is trusted only
// as a regular file (not a symlink) in a directory, and itself, that nobody
// but the owner can write, owned by root, by the caller, or by the owner of
// the install's binary -- whoever owns the binary can already change what
// every job runs. The checks are made on the open descriptor the path is then
// read from, so the file cannot be swapped in between.
func ReadPointer(prefix string) (target string, ok bool, err error) {
	path := filepath.Join(prefix, PointerRel)
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("config pointer %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return "", false, fmt.Errorf("config pointer %s: not a regular file; refusing to trust it", path)
	}
	binUID, binKnown := -1, false
	if bi, err := os.Stat(filepath.Join(prefix, "bin", "slurm-shim")); err == nil {
		binUID, binKnown = ownerUID(bi)
	}
	euid := os.Geteuid()
	if di, err := os.Lstat(filepath.Dir(path)); err != nil {
		return "", false, fmt.Errorf("config pointer %s: %w", path, err)
	} else if err := trusted(di, euid, binUID, binKnown); err != nil {
		return "", false, fmt.Errorf("config pointer %s: its directory %s; refusing to trust it", path, err)
	}

	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false, fmt.Errorf("config pointer %s: %w; refusing to trust it", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err = f.Stat()
	if err != nil {
		return "", false, fmt.Errorf("config pointer %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return "", false, fmt.Errorf("config pointer %s: not a regular file; refusing to trust it", path)
	}
	if err := trusted(fi, euid, binUID, binKnown); err != nil {
		return "", false, fmt.Errorf("config pointer %s: %s; refusing to trust it", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPointerBytes))
	if err != nil {
		return "", false, fmt.Errorf("config pointer %s: %w", path, err)
	}
	target = strings.TrimSpace(string(data))
	if target == "" || strings.ContainsAny(target, "\n\r") || !filepath.IsAbs(target) {
		return "", false, fmt.Errorf("config pointer %s: must hold one absolute path, found %q", path, target)
	}
	return filepath.Clean(target), true, nil
}

// WritePointer points the install at prefix to target. It writes beside and
// renames over (0644: every job user reads it), so a planted symlink is
// replaced rather than followed and a job never reads half a path.
func WritePointer(prefix, target string) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("config pointer target %q is not an absolute path", target)
	}
	path := filepath.Join(prefix, PointerRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-path-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(filepath.Clean(target) + "\n"); err != nil {
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

// RemovePointer removes the install's pointer, if any.
func RemovePointer(prefix string) error {
	err := os.Remove(filepath.Join(prefix, PointerRel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// present reports whether path exists; only "does not exist" is false, any
// other failure (a directory this user cannot search) is an error.
func present(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// readable opens path the way a job will, so a file the caller cannot read is
// caught here rather than with an error that does not name the pointer.
func readable(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	return nil
}

func ownerUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// trusted describes, as an error, why fi (the pointer or its directory) cannot
// be trusted: writable by group or others, or owned by someone else than root,
// the caller or the install binary's owner.
func trusted(fi os.FileInfo, euid, binUID int, binKnown bool) error {
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("writable by group or others (%v)", fi.Mode().Perm())
	}
	if uid, known := ownerUID(fi); known && !trustedOwner(uid, euid, binUID, binKnown) {
		return fmt.Errorf("owned by uid %d, not root, you, or the owner of the install's binary", uid)
	}
	return nil
}

// trustedOwner is the owner rule for the pointer and its directory.
func trustedOwner(uid, euid, binUID int, binKnown bool) bool {
	return uid == 0 || uid == euid || (binKnown && uid == binUID)
}
