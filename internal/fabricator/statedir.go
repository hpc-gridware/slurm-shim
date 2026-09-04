package fabricator

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// EnsureStateDir returns the per-job state directory under tmpdir, creating it
// when absent, and refuses to hand back anything a co-tenant could have planted.
//
// The per-job TMPDIR sits at a predictable path (/tmp/<job>.<task>.<queue>) in a
// world-writable /tmp, and Open Cluster Scheduler tolerates a pre-existing
// directory there: it chowns it to the job owner without resetting the mode. A
// co-tenant can therefore hand the job a TMPDIR they still have write access to
// and plant a state directory, a failure sentinel or an environment file inside
// it. Everything the sourcing hook later trusts lives here, so this is the trust
// boundary on the writing side:
//
//   - tmpdir must be a real directory (not a symlink) owned by the current user.
//     Group/world write is stripped -- it is the job's own directory, and the
//     owner reclaiming it closes the co-tenant's window.
//   - the state directory must be a real directory owned by the current user with
//     no group/world write. Anything else at that path is renamed aside (rename
//     needs write on the parent only, which is ours) and a fresh 0700 directory
//     is created in its place.
//
// The hook performs the mirror-image checks before sourcing (REQ-FAB-010).
func EnsureStateDir(tmpdir string) (string, error) {
	if tmpdir == "" {
		return "", errors.New("TMPDIR is not set; refusing to write job state to a shared path")
	}
	if err := reclaimOwnedDir(tmpdir); err != nil {
		return "", fmt.Errorf("TMPDIR %s %v", tmpdir, err)
	}
	dir := StateDir(tmpdir)
	err := os.Mkdir(dir, 0o700)
	if err == nil {
		return dir, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if privateDir(dir) == nil {
		return dir, nil
	}
	aside := fmt.Sprintf("%s.untrusted.%d", dir, os.Getpid())
	if err := os.Rename(dir, aside); err != nil {
		return "", fmt.Errorf("state dir %s is not private to this job and could not be moved aside: %w", dir, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// reclaimOwnedDir verifies path is a real directory owned by the current user
// and strips group/world write from it.
func reclaimOwnedDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if !fi.IsDir() {
		return errors.New("is not a directory")
	}
	if !ownedByUs(fi) {
		return errors.New("is not owned by the current user")
	}
	if fi.Mode().Perm()&0o022 != 0 {
		if err := os.Chmod(path, fi.Mode().Perm()&^0o022); err != nil {
			return fmt.Errorf("could not remove group/world write: %w", err)
		}
	}
	return nil
}

// privateDir reports (as a nil error) that path is a real directory owned by
// the current user with no group/world write.
func privateDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return errors.New("symlink")
	case !fi.IsDir():
		return errors.New("not a directory")
	case !ownedByUs(fi):
		return errors.New("foreign owner")
	case fi.Mode().Perm()&0o022 != 0:
		return errors.New("group/world writable")
	}
	return nil
}

func ownedByUs(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Geteuid()
}
