//go:build !linux

package stepper

// setAffinity is a best-effort no-op off Linux (SI-34); macOS has no
// sched_setaffinity, and standalone/dev runs there do not need binding.
func setAffinity(cpuset string) error {
	_ = cpuset
	return nil
}
