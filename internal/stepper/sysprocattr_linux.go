//go:build linux

package stepper

import "syscall"

// rankSysProcAttr places each rank in its own process group (so the stepper can
// signal the whole rank tree, REQ-STP-004) and requests SIGTERM if the stepper
// dies (Pdeathsig), a kernel-enforced orphan guard on top of the control-channel
// EOF check (SI-08). The stepper's spawn goroutine must not lock its OS thread,
// or a locked thread's death would spuriously trip Pdeathsig.
func rankSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
