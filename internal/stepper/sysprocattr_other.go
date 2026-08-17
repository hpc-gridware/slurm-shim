//go:build !linux

package stepper

import "syscall"

// rankSysProcAttr places each rank in its own process group. Pdeathsig is
// Linux-only, so off Linux the orphan guard relies on the control-channel EOF
// check (SI-08) alone.
func rankSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
