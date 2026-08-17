//go:build linux

package stepper

import "golang.org/x/sys/unix"

// setAffinity pins the calling thread to the CPUs in cpuset via
// sched_setaffinity (REQ-STP-002). The caller must have locked the OS thread so
// the mask and the subsequent exec share one thread.
func setAffinity(cpuset string) error {
	cpus, err := parseCPUList(cpuset)
	if err != nil {
		return err
	}
	var set unix.CPUSet
	set.Zero()
	for _, c := range cpus {
		set.Set(c)
	}
	return unix.SchedSetaffinity(0, &set)
}
