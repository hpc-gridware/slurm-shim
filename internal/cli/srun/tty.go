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

package srun

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// stdinIsTerminal reports whether stdin is a terminal. Asking for its
// foreground process group succeeds only on a terminal: /dev/null is a
// character device too, but not one.
func stdinIsTerminal() bool {
	_, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	return err == nil
}

// backgroundOfTerminal reports whether stdin is a terminal this process does
// not own: srun started with & from an interactive shell. Reading that terminal
// would stop the process (SIGTTIN), so its session gets no stdin.
func backgroundOfTerminal() bool {
	if !stdinIsTerminal() {
		return false
	}
	fg, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	return err == nil && fg != syscall.Getpgrp()
}
