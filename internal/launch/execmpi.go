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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"syscall"
	"time"
)

// The process seam for srun --mpi=pmix (REQ-IMP-001): srun asks the MPI
// launcher what it is, then replaces itself with it; each rank's rank-env
// trampoline replaces itself with the rank's command.

// ErrCommandNotFound is the error when the command is not on PATH.
var ErrCommandNotFound = errors.New("command not found")

// versionProbeTimeout bounds `<launcher> --version`, so a launcher that hangs
// cannot hang srun. A variable so a spec can shorten it.
var versionProbeTimeout = 10 * time.Second

// ProbeVersion resolves name on PATH and returns its path and the output of
// `<path> --version`. A non-zero exit with output still returns the output:
// some launchers print their version and exit 1.
func ProbeVersion(name string) (path, out string, err error) {
	path, err = exec.LookPath(name)
	if notFound(err) {
		return "", "", fmt.Errorf("%s: %w", name, ErrCommandNotFound)
	}
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// A wrapper script's grandchild can hold the output pipe after the
	// wrapper is killed; stop waiting for it.
	cmd.WaitDelay = time.Second
	b, err := cmd.CombinedOutput()
	if err != nil && len(b) == 0 {
		return path, "", fmt.Errorf("%s --version: %w", path, err)
	}
	return path, string(b), nil
}

// notFound reports a LookPath failure that means no such command: not on
// PATH, or a path that does not exist. Other failures (not executable) are
// passed on, so a caller can tell them apart (exit 127 vs 126).
func notFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

// Exec replaces the process with name (looked up on PATH unless it contains a
// slash), argv and env. It returns only on failure.
func Exec(name string, argv, env []string) error {
	path, err := exec.LookPath(name)
	if notFound(err) {
		return fmt.Errorf("%s: %w", name, ErrCommandNotFound)
	}
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}
