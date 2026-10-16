/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

package launch

import (
	"fmt"
	"io"
	"sync"

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
		return QrshLauncher{Self: self, Stderr: stderr, slotNotice: new(sync.Once)}, nil
	case "local":
		return LocalLauncher{Self: self, Stderr: stderr}, nil
	case "ssh":
		return nil, fmt.Errorf("launcher %q is not implemented in this build (stretch scope)", cfg.Launcher)
	default:
		return nil, fmt.Errorf("unknown launcher %q", cfg.Launcher)
	}
}
