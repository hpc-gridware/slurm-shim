//go:build linux

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

package cpubind

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// sysCPU holds the node's CPU facts. Its online list ("0-13") is the node's
// CPU count: runtime.NumCPU is no substitute, it counts the calling process's
// affinity mask.
const sysCPU = "/sys/devices/system/cpu"

// ThisNode returns what this node offers a step (see Node). Only the mask is
// required: an unreadable online list or topology leaves Online or the core
// order unknown, which ShouldPin and Owned handle. Masks wider than 1024 CPUs
// (unix.CPUSet) fail here and leave the tasks unpinned.
func ThisNode() (Node, error) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return Node{}, err
	}
	var allowed []int
	coreOf := map[int]int{}
	for c := 0; c < len(set)*64; c++ {
		if !set.IsSet(c) {
			continue
		}
		allowed = append(allowed, c)
		if siblings, err := readList(fmt.Sprintf("%s/cpu%d/topology/thread_siblings_list", sysCPU, c)); err == nil {
			coreOf[c] = siblings[0]
		}
	}
	n := Node{}
	n.Allowed, n.Cores = CoreFirst(allowed, coreOf)
	if online, err := readList(sysCPU + "/online"); err == nil {
		n.Online = len(online)
	}
	return n, nil
}

// readList reads a sysfs CPU list file.
func readList(path string) ([]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseList(strings.TrimSpace(string(data)))
}

// Set pins the calling thread to the CPUs in cpus via sched_setaffinity
// (REQ-STP-002). The caller must have locked the OS thread so the mask and
// the subsequent exec share one thread.
func Set(cpus string) error {
	list, err := ParseList(cpus)
	if err != nil {
		return err
	}
	var set unix.CPUSet
	set.Zero()
	for _, c := range list {
		set.Set(c)
	}
	return unix.SchedSetaffinity(0, &set)
}
