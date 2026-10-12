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
