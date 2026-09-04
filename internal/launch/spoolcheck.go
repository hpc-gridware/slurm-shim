package launch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// tokenSpoolWarning inspects where Grid Engine will stage the per-step token and
// returns the warning to emit, or "" when the staging area is not reachable by
// other users.
//
// The token travels to each remote stepper via `qrsh -v` (REQ-CHN-002), which GE
// implements by writing it into the petask's environment file under the execd
// spool. Its confidentiality therefore rests on that directory's permissions,
// which belong to the scheduler rather than to the shim -- so this used to be an
// unconditional "go and check" warning on every remote srun. It is checkable:
// if any directory on the path to the spool denies other users traversal, no
// co-tenant can reach the file and there is nothing to report.
//
// The check is local to the host running srun (also an exec host), and a
// site with per-host spool policies could still differ elsewhere; when the path
// cannot be determined the original advisory is returned unchanged.
func tokenSpoolWarning(ctx context.Context, r gedata.Runner) string {
	const advise = "token delivered via qrsh -v: confirm the execd env spool file is owner-only for the step lifetime (SI-51)"

	dir := execdSpoolDir(ctx, r)
	if dir == "" {
		return advise
	}
	open, why := othersCanTraverse(dir)
	if !open {
		return "" // unreachable by other users: nothing to confirm
	}
	if why == "" {
		return advise
	}
	return fmt.Sprintf(
		"token delivered via qrsh -v lands in the execd spool, and %s is traversable by other users (%s): "+
			"a co-tenant can read the step token while the step runs (SI-51)", dir, why)
}

// execdSpoolDir reads execd_spool_dir from the global configuration. An empty
// result means "could not determine", never "not set".
func execdSpoolDir(ctx context.Context, r gedata.Runner) string {
	stdout, _, exit, err := r.Run(ctx, "qconf", "-sconf")
	if err != nil || exit != 0 {
		return ""
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "execd_spool_dir" {
			return fields[1]
		}
	}
	return ""
}

// othersCanTraverse reports whether every directory from the filesystem root to
// dir grants other-execute, which is what a co-tenant needs to reach anything
// underneath. why carries dir's own mode for the diagnostic, since that is the
// permission an administrator would change. A component that cannot be stat'ed
// is treated as unknown rather than safe: the shim cannot vouch for a path it
// cannot see, so the caller keeps the generic advisory.
func othersCanTraverse(dir string) (open bool, why string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return true, ""
	}
	self, err := os.Stat(abs)
	if err != nil {
		return true, ""
	}
	for p := abs; ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err != nil {
			return true, ""
		}
		if fi.Mode().Perm()&0o001 == 0 {
			return false, "" // a closed component: nothing below it is reachable
		}
		if parent := filepath.Dir(p); parent == p {
			break
		}
	}
	return true, fmt.Sprintf("mode %04o", self.Mode().Perm())
}
