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
