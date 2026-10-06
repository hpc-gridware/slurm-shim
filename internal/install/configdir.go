package install

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// QontrolMarker is the file Qontrol keeps at the top of a config dir it has
// initialized (qontrol serve --config-dir). Qontrol refuses to start on a
// non-empty directory without it, so the shim never writes into one that lacks
// it.
const QontrolMarker = ".qontrol-dir.json"

// ConfigDirSub is the shim's directory inside a Qontrol config dir, beside
// Qontrol's own top-level directories (license-manager/ is the precedent).
const ConfigDirSub = "slurm-shim"

// configRel is the config inside a Qontrol config dir, relative to it.
var configRel = path.Join(ConfigDirSub, "config.yaml")

// maxConfigBytes bounds a config read as root: a config is a few KiB.
const maxConfigBytes = 4 << 20

// configDirPathRe is Qontrol's config-dir character set; a path Qontrol would
// refuse at startup is refused here too.
var configDirPathRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// ConfigDir is a validated Qontrol config dir: where the shim's config goes and
// who must be able to edit it.
//
// The directory belongs to Qontrol's user, who can rename, link and replace
// anything in it at any time. So the installer, which may run as root, never
// touches it by path: every read, write, chown and removal goes through an
// os.Root opened on Dir (no symlink leads out of it), and the mode and owner
// of a written file are set on its descriptor, never on a name.
type ConfigDir struct {
	Dir    string
	Config string // Dir/slurm-shim/config.yaml
	UID    int    // Dir's owner: Qontrol's user
	GID    int
	EUID   int // the installing user
}

// CheckConfigDir validates dir for `install --config-dir` before anything is
// changed. cellCommon is $SGE_ROOT/$SGE_CELL/common ("" when unknown); euid is
// the installing user. Any user who can write the config decides what every
// job runs (drain_command), so the rules are the install tree's: nothing on
// the way to the config is writable by group or others, and the installer
// must be root or the dir's owner, or Qontrol could not edit what it manages.
func CheckConfigDir(dir, cellCommon string, euid int) (ConfigDir, error) {
	if !filepath.IsAbs(dir) {
		return ConfigDir{}, fmt.Errorf("--config-dir %q is not an absolute path", dir)
	}
	dir = filepath.Clean(dir)
	if !configDirPathRe.MatchString(dir) {
		return ConfigDir{}, fmt.Errorf("--config-dir %q contains characters a Qontrol config dir cannot have "+
			"(letters, digits, / . _ - only)", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return ConfigDir{}, fmt.Errorf("--config-dir: %w", err)
	}
	if !fi.IsDir() {
		return ConfigDir{}, fmt.Errorf("--config-dir %s is not a directory", dir)
	}
	if m, err := os.Lstat(filepath.Join(dir, QontrolMarker)); err != nil || !m.Mode().IsRegular() {
		return ConfigDir{}, fmt.Errorf("--config-dir %s is not a Qontrol config dir (no %s): start "+
			"`qontrol serve --config-dir %s` once first, or use --config PATH for any other location",
			dir, QontrolMarker, dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return ConfigDir{}, fmt.Errorf("--config-dir %s is writable by group or others (%v); the config decides "+
			"what every job runs, so only its owner may write there", dir, fi.Mode().Perm())
	}
	if p := writableAncestor(dir); p != "" {
		return ConfigDir{}, fmt.Errorf("--config-dir %s: %s is writable by group or others and not sticky, so "+
			"anyone in that group can replace the config dir", dir, p)
	}
	if cellCommon != "" && Within(dir, cellCommon) {
		return ConfigDir{}, fmt.Errorf("--config-dir %s is inside the cell's common directory %s; a Qontrol "+
			"config dir lives outside it so it survives a reinstall", dir, cellCommon)
	}
	uid, gid, ok := owner(fi)
	if !ok {
		return ConfigDir{}, fmt.Errorf("--config-dir %s: cannot read its owner", dir)
	}
	cd := ConfigDir{Dir: dir, Config: filepath.Join(dir, configRel), UID: uid, GID: gid, EUID: euid}
	if euid != 0 && euid != cd.UID {
		return ConfigDir{}, fmt.Errorf("--config-dir %s is owned by %s, but this install runs as %s: run it as "+
			"%s, or as root, so Qontrol can edit the config", dir, userName(cd.UID), userName(euid), userName(cd.UID))
	}
	sub := filepath.Join(dir, ConfigDirSub)
	s, err := os.Lstat(sub)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return ConfigDir{}, err
	case !s.IsDir():
		return ConfigDir{}, fmt.Errorf("%s exists and is not a directory", sub)
	case s.Mode().Perm()&0o022 != 0:
		return ConfigDir{}, fmt.Errorf("%s is writable by group or others (%v); make it 0755", sub, s.Mode().Perm())
	default:
		if u, _, ok := owner(s); ok && u != cd.UID && u != 0 {
			return ConfigDir{}, fmt.Errorf("%s is owned by %s, not by the config dir's owner %s",
				sub, userName(u), userName(cd.UID))
		}
	}
	return cd, nil
}

// ReadConfig returns the config's content; an error satisfying
// errors.Is(err, fs.ErrNotExist) when there is none. Read as root, a file Q
// hardlinked in from elsewhere would be disclosed, so only a regular file with
// a single link is read.
func (cd ConfigDir) ReadConfig() ([]byte, error) {
	r, err := os.OpenRoot(cd.Dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	f, err := r.OpenFile(configRel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !fi.Mode().IsRegular() || (ok && st.Nlink != 1) {
		return nil, fmt.Errorf("%s is not a regular file with one link; refusing to read it", cd.Config)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes; not a slurm-shim config", cd.Config, maxConfigBytes)
	}
	return data, nil
}

// WriteConfig replaces the config atomically: a new file beside it, fsynced,
// renamed over it. The mode carries over from the file it replaces, narrowed
// to at most 0644 (whoever owns the dir chose it); as root, the file and
// slurm-shim/ are given to the dir's owner.
func (cd ConfigDir) WriteConfig(data []byte) error {
	r, err := os.OpenRoot(cd.Dir)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	if err := r.Mkdir(ConfigDirSub, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if s, err := r.Lstat(ConfigDirSub); err != nil {
		return err
	} else if !s.IsDir() {
		return fmt.Errorf("%s was replaced by something that is not a directory; nothing written",
			filepath.Join(cd.Dir, ConfigDirSub))
	}
	if cd.EUID == 0 {
		if err := r.Lchown(ConfigDirSub, cd.UID, cd.GID); err != nil {
			return err
		}
	}
	mode := os.FileMode(0o644)
	if fi, err := r.Lstat(configRel); err == nil && fi.Mode().IsRegular() {
		mode = fi.Mode().Perm() & 0o644
	}

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := path.Join(ConfigDirSub, ".slurm-shim-config-"+hex.EncodeToString(suffix))
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = r.Remove(tmp) }() // no-op once renamed
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if cd.EUID == 0 {
		if err := f.Chown(cd.UID, cd.GID); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return r.Rename(tmp, configRel)
}

// RemoveConfig removes the config, then slurm-shim/ when that is empty.
func (cd ConfigDir) RemoveConfig() error {
	r, err := os.OpenRoot(cd.Dir)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	fi, err := r.Lstat(configRel)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; the config was not touched", cd.Config)
	}
	if err := r.Remove(configRel); err != nil {
		return err
	}
	_ = r.Remove(ConfigDirSub) // only when empty; a site's own files keep it
	return nil
}

// OpenConfigDir returns the ConfigDir a config path lies in, when it is the
// shim config inside a Qontrol config dir (Dir/slurm-shim/config.yaml with the
// marker in Dir). The owner is read from Dir; nothing is validated beyond
// that, so an existing install keeps working however the dir was changed --
// writes still go through the dir's os.Root.
func OpenConfigDir(cfgPath string, euid int) (ConfigDir, bool) {
	cfgPath = filepath.Clean(cfgPath)
	sub := filepath.Dir(cfgPath)
	if filepath.Base(cfgPath) != "config.yaml" || filepath.Base(sub) != ConfigDirSub {
		return ConfigDir{}, false
	}
	dir := filepath.Dir(sub)
	if m, err := os.Lstat(filepath.Join(dir, QontrolMarker)); err != nil || !m.Mode().IsRegular() {
		return ConfigDir{}, false
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return ConfigDir{}, false
	}
	uid, gid, ok := owner(fi)
	if !ok {
		return ConfigDir{}, false
	}
	return ConfigDir{Dir: dir, Config: cfgPath, UID: uid, GID: gid, EUID: euid}, true
}

// writableAncestor returns the first directory above dir (resolved) that
// group or others can write without the sticky bit, "" when there is none.
func writableAncestor(dir string) string {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	for d := filepath.Dir(real); ; d = filepath.Dir(d) {
		if fi, err := os.Lstat(d); err == nil && fi.Mode().Perm()&0o022 != 0 && !stickyDir(fi) {
			return d
		}
		if d == filepath.Dir(d) {
			return ""
		}
	}
}

// Within reports whether path is base or below it, lexically or after
// resolving symlinks.
func Within(path, base string) bool {
	in := func(p, b string) bool {
		return p == b || strings.HasPrefix(p, b+string(filepath.Separator))
	}
	if in(filepath.Clean(path), filepath.Clean(base)) {
		return true
	}
	rp, errP := filepath.EvalSymlinks(path)
	rb, errB := filepath.EvalSymlinks(base)
	return errP == nil && errB == nil && in(rp, rb)
}

// SamePath reports whether two paths name the same file or directory,
// lexically or after following symlinks.
func SamePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func owner(fi os.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

func userName(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return "uid " + strconv.Itoa(uid)
}
