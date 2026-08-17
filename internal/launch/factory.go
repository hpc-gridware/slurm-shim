package launch

import (
	"fmt"
	"io"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// For returns the launcher for slave (non-master) hosts per cfg.Launcher. The
// master host always runs under the LocalLauncher (REQ-RUN-012), so callers use
// this only for the remaining hosts.
//
//	"qrsh-inherit" (default) -> QrshLauncher (GE tight integration)
//	"local"                  -> LocalLauncher (single-node, dev, and the test suite)
//	"ssh"                    -> unsupported in this build (stretch scope, SI-46)
func For(cfg *config.Config, self string, stderr io.Writer) (Launcher, error) {
	switch cfg.Launcher {
	case "", "qrsh-inherit":
		return QrshLauncher{Self: self, Stderr: stderr}, nil
	case "local":
		return LocalLauncher{Self: self, Stderr: stderr}, nil
	case "ssh":
		return nil, fmt.Errorf("launcher %q is not implemented in this build (stretch scope)", cfg.Launcher)
	default:
		return nil, fmt.Errorf("unknown launcher %q", cfg.Launcher)
	}
}
