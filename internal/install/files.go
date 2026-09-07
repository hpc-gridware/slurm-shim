package install

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Commands are the SLURM command names (and the shim's own helpers) linked to
// the binary. Kept in one place with the dispatch table in cmd/slurm-shim.
var Commands = []string{
	"srun", "sbatch", "sacct", "squeue", "scancel", "scontrol", "sinfo",
	"slurm-shim-env", "slurm-shim-stepper",
}

// Payload files, relative to a payload or prefix root.
const (
	BinaryRel  = "bin/slurm-shim"
	StarterRel = "bin/slurm-shim-starter"
	HookRel    = "etc/slurm-shim-source-hook.sh"
)

// InstallTree places the runtime tree at prefix from the payload at src (a
// package payload, an unpacked tarball, or a build tree). It copies the binary,
// the starter and the hook, creates the command symlinks, and sets the modes the
// starter's trust boundary needs. It never chowns: the caller runs as whoever
// should own the tree (root, or the OCS admin user on a root-squashed share),
// and CheckTree then verifies the result instead of assuming it.
//
// src == prefix means the tree is already in place (a tarball unpacked straight
// into $SGE_ROOT/slurm-shim); only the links and modes are (re)applied.
func InstallTree(src, prefix string) error {
	if src == "" || prefix == "" {
		return fmt.Errorf("install: source and prefix are required")
	}
	same, err := samePath(src, prefix)
	if err != nil {
		return err
	}
	for _, d := range []string{"bin", "etc", "share"} {
		if err := os.MkdirAll(filepath.Join(prefix, d), 0o755); err != nil {
			return fmt.Errorf("install: %w", err)
		}
	}
	files := []struct {
		rel  string
		mode os.FileMode
	}{
		{BinaryRel, 0o755},
		{StarterRel, 0o755},
		{HookRel, 0o644},
	}
	for _, f := range files {
		dst := filepath.Join(prefix, f.rel)
		if !same {
			if err := copyFile(filepath.Join(src, f.rel), dst, f.mode); err != nil {
				return err
			}
		}
		if err := os.Chmod(dst, f.mode); err != nil {
			return fmt.Errorf("install: chmod %s: %w", dst, err)
		}
	}
	// Relative links, so bin/ copies as a unit (README Quickstart).
	for _, c := range Commands {
		link := filepath.Join(prefix, "bin", c)
		_ = os.Remove(link)
		if err := os.Symlink("slurm-shim", link); err != nil {
			return fmt.Errorf("install: link %s: %w", link, err)
		}
	}
	return nil
}

// copyFile copies src to dst atomically (write beside, rename over) so a job
// starting mid-upgrade never sees a half-written starter or binary.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("install: payload %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("install: copying to %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install: %w", err)
	}
	return nil
}

func samePath(a, b string) (bool, error) {
	ra, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	rb, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	return filepath.Clean(ra) == filepath.Clean(rb), nil
}

// Problem is one thing CheckTree found wrong with the install tree.
type Problem struct {
	Path string
	Why  string
}

// CheckTree verifies the trust boundary the starter depends on: it runs as the
// job user for every job in the queue, so every file it executes and every
// directory on the way to them must be owned by a principal that can already act
// as any job user (root, or the OCS admin user) and writable by nobody else.
// adminUser is "" when the cell's bootstrap says none (then only root qualifies).
func CheckTree(prefix, adminUser string) []Problem {
	allowed := map[string]bool{"root": true}
	if adminUser != "" {
		allowed[adminUser] = true
	}

	// Resolve the prefix before walking its ancestors. A component may legitimately
	// be a symlink -- macOS has /var -> private/var, and a site may point
	// /opt/ocs at shared storage -- and walking the lexical path would inspect
	// directories that are not on the real chain while missing the ones that are.
	root, err := filepath.EvalSymlinks(prefix)
	if err != nil {
		return []Problem{{prefix, "cannot resolve: " + err.Error()}}
	}

	var out []Problem
	check := func(path string, fi os.FileInfo) {
		if fi.Mode().Perm()&0o022 != 0 && !stickyDir(fi) {
			out = append(out, Problem{path, "group- or world-writable (mode " + modeText(fi) + ")"})
		}
		if owner := ownerName(fi); !allowed[owner] {
			out = append(out, Problem{path, "owned by " + owner + "; must be root" + orAdmin(adminUser)})
		}
	}

	// The files the starter executes or sources. A symlink here would extend the
	// trust chain somewhere this walk cannot see, and InstallTree never creates
	// one, so treat it as a problem rather than following it.
	for _, rel := range []string{BinaryRel, StarterRel, HookRel} {
		path := filepath.Join(root, rel)
		fi, err := os.Lstat(path)
		if err != nil {
			out = append(out, Problem{path, "missing: " + err.Error()})
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			out = append(out, Problem{path, "is a symlink; the trust chain must be a real file"})
			continue
		}
		check(path, fi)
	}

	// Every directory from the tree up to the filesystem root. These are real
	// directories by construction now, so ownership and mode are the whole test.
	for d := root; ; d = filepath.Dir(d) {
		fi, err := os.Lstat(d)
		if err != nil {
			out = append(out, Problem{d, "missing: " + err.Error()})
		} else {
			check(d, fi)
		}
		if d == filepath.Dir(d) {
			break
		}
	}

	// The command names are relative symlinks to the binary, by design.
	for _, c := range Commands {
		link := filepath.Join(root, "bin", c)
		if target, err := os.Readlink(link); err != nil || target != "slurm-shim" {
			out = append(out, Problem{link, "missing or not a link to slurm-shim"})
		}
	}
	return out
}

// stickyDir reports whether a world-writable DIRECTORY is nonetheless safe to
// have on the path to the starter. The sticky bit stops anyone but a file's
// owner renaming or removing it, which is exactly the substitution this check
// exists to prevent -- /tmp is 1777 for that reason. An attacker can still
// create their own entries beside ours, but cannot replace ours, so the chain
// holds. Meaningless for files: writable is writable.
func stickyDir(fi os.FileInfo) bool {
	return fi.IsDir() && fi.Mode()&os.ModeSticky != 0
}

// modeText renders the permission bits including the sticky bit, which
// os.FileMode.Perm() drops -- without it a report cannot tell 0777 (a real
// hazard) from 1777 (not one).
func modeText(fi os.FileInfo) string {
	perm := uint32(fi.Mode().Perm())
	if fi.Mode()&os.ModeSticky != 0 {
		perm |= 0o1000
	}
	return fmt.Sprintf("%04o", perm)
}

func orAdmin(adminUser string) string {
	if adminUser == "" {
		return ""
	}
	return " or " + adminUser
}

// ownerName resolves a file's owner to a name, falling back to the uid.
func ownerName(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "?"
	}
	uid := strconv.FormatUint(uint64(st.Uid), 10)
	if u, err := user.LookupId(uid); err == nil {
		return u.Username
	}
	return uid
}

// AdminUser reads the cell's admin_user from $SGE_ROOT/$SGE_CELL/common/bootstrap.
// "none" (the default) means root does everything, returned as "".
func AdminUser(sgeRoot, cell string) (string, error) {
	if cell == "" {
		cell = "default"
	}
	f, err := os.Open(filepath.Join(sgeRoot, cell, "common", "bootstrap"))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 2 && fs[0] == "admin_user" {
			if strings.EqualFold(fs[1], "none") {
				return "", nil
			}
			return fs[1], nil
		}
	}
	return "", sc.Err()
}

// ExposeMode says how the commands reach users' PATH.
type ExposeMode string

const (
	ExposeNone     ExposeMode = "none"
	ExposeModule   ExposeMode = "module"
	ExposeProfileD ExposeMode = "profile.d"
)

// Expose writes the PATH hook for the chosen mode and returns the file written
// (or "" for none). The modulefile is Tcl, which Lmod and environment-modules
// both read; it lands under the prefix so the admin adds one directory to
// MODULEPATH. profile.d is written straight to /etc/profile.d, which is why it
// is opt-in: it exposes sbatch/srun to every login shell on the host.
func Expose(prefix string, mode ExposeMode, version string) (string, error) {
	bin := filepath.Join(prefix, "bin")
	switch mode {
	case ExposeNone, "":
		return "", nil
	case ExposeModule:
		dir := filepath.Join(prefix, "share", "modulefiles", "slurm-shim")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		path := filepath.Join(dir, version)
		body := "#%Module1.0\n" +
			"## slurm-shim: SLURM command compatibility for Open Cluster Scheduler\n" +
			"proc ModulesHelp { } { puts stderr \"sbatch/srun/squeue/... translated to OCS\" }\n" +
			"module-whatis \"SLURM commands for Open Cluster Scheduler\"\n" +
			"prepend-path PATH " + bin + "\n"
		return path, os.WriteFile(path, []byte(body), 0o644)
	case ExposeProfileD:
		path := "/etc/profile.d/slurm-shim.sh"
		body := "# slurm-shim: SLURM commands for Open Cluster Scheduler\n" +
			"export PATH=" + bin + ":$PATH\n"
		return path, os.WriteFile(path, []byte(body), 0o644)
	}
	return "", fmt.Errorf("install: unknown expose mode %q (none, module, profile.d)", mode)
}
