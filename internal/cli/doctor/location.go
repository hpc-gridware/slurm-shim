package doctor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/install"
)

// locationWarnings are the WARN lines about where the config lives: a cell
// config this install ignores through its pointer, a config (or a directory
// holding it) others can write -- it decides what every job runs --, a config
// job users cannot read, and an install record naming a different file than
// the pointer.
func locationWarnings(prefix string, loc config.Location) []string {
	var out []string
	if loc.Source == config.SourcePointer {
		if cell := config.CellPath(); cell != loc.Path {
			if _, err := os.Stat(cell); err == nil {
				// Not "remove it": another install on the cell, without a pointer,
				// may still read it -- install keeps it for exactly that reason.
				out = append(out, fmt.Sprintf("config: %s exists but this install reads %s through its pointer; "+
					"edits to the cell file do not reach it (installs without a pointer still read the cell file)",
					cell, loc.Path))
			}
		}
		if st, err := install.ReadState(prefix); err == nil && st != nil && st.Config != "" && st.Config != loc.Path {
			out = append(out, fmt.Sprintf("config: the install record names %s but the pointer names %s; "+
				"re-run slurm-shim install --apply to make them agree", st.Config, loc.Path))
		}
	}
	if !loc.Exists {
		return out
	}
	if fi, err := os.Stat(loc.Path); err == nil {
		if fi.Mode().Perm()&0o022 != 0 {
			out = append(out, fmt.Sprintf("config: %s is writable by group or others (%v); whoever can write it "+
				"decides what every job runs", loc.Path, fi.Mode().Perm()))
		}
		if fi.Mode().Perm()&0o004 == 0 {
			out = append(out, fmt.Sprintf("config: %s is not readable by others (%v); jobs run as their users, "+
				"and every one of them must read it", loc.Path, fi.Mode().Perm()))
		}
	}
	// The directories holding it: whoever can write one can replace the file.
	for d, i := filepath.Dir(loc.Path), 0; i < 2; d, i = filepath.Dir(d), i+1 {
		if di, err := os.Stat(d); err == nil && di.Mode().Perm()&0o022 != 0 && di.Mode()&os.ModeSticky == 0 {
			out = append(out, fmt.Sprintf("config: %s, which holds the config, is writable by group or others "+
				"(%v); anyone who can write it can replace the config", d, di.Mode().Perm()))
		}
	}
	return out
}
