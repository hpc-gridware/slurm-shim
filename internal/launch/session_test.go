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

package launch_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/launch"
)

// SessionArgs is the single qrsh-argv builder for an interactive session; the
// exec and the dry run both read it, so these specs pin the exact verified argv.
var _ = Describe("interactive SessionArgs [srun --pty]", func() {
	base := launch.SessionSpec{Pty: true, Queue: "all.q", PE: "make", Slots: 2, Command: []string{"bash"}}

	joined := func(s launch.SessionSpec) string { return strings.Join(launch.SessionArgs(s), " ") }

	It("always requests a queued, pty session in the invocation dir", func() {
		// -now no (queue and wait), -pty y (force a terminal), -cwd (SLURM's
		// working dir) -- all verified required on OCS 9.1.5.
		Expect(joined(base)).To(HavePrefix("-now no -pty y -cwd -q all.q -pe make 2"))
	})

	It("passes no -pty flag at all without a terminal, and -now yes for -I", func() {
		s := base
		s.Pty = false
		// An explicit -pty no failed every job in the shepherd and put the queue
		// instance into error state (verified, OCS 9.1.6); the default is right.
		Expect(joined(s)).To(HavePrefix("-now no -cwd -q all.q"))
		Expect(launch.SessionArgs(s)).NotTo(ContainElement("-pty"))
		s.Immediate = true
		Expect(joined(s)).To(HavePrefix("-now yes -cwd"))
	})

	It("ends with the command, verbatim and last", func() {
		s := base
		s.Command = []string{"python", "-c", "print('a b')"}
		args := launch.SessionArgs(s)
		Expect(args[len(args)-3:]).To(Equal([]string{"python", "-c", "print('a b')"}))
	})

	It("maps --chdir to -wd instead of -cwd", func() {
		s := base
		s.Chdir = "/work/sub"
		Expect(joined(s)).To(ContainSubstring("-wd /work/sub"))
		Expect(joined(s)).NotTo(ContainSubstring("-cwd"))
	})

	It("emits -A for the account and -N for the job name", func() {
		s := base
		s.Account, s.JobName = "proj1", "debug"
		Expect(joined(s)).To(ContainSubstring("-A proj1"))
		Expect(joined(s)).To(ContainSubstring("-N debug"))
	})

	It("emits -l for the resource list and -par -w e for a pinned layout", func() {
		s := base
		s.Resources = "h_rt=1800,gpu=1"
		s.AllocationRule = "$pe_slots"
		s.VerifyGeometry = true // -w e rides along only when it can judge the request
		Expect(joined(s)).To(ContainSubstring("-l h_rt=1800,gpu=1"))
		Expect(joined(s)).To(ContainSubstring("-par $pe_slots -w e"))
	})

	It("passes the export flags through", func() {
		s := base
		s.Export = []string{"-V", "-v", "FOO=bar"}
		Expect(joined(s)).To(ContainSubstring("-V -v FOO=bar bash"))
	})

	It("omits the optional flags when unset", func() {
		out := joined(base)
		for _, flag := range []string{"-wd", "-par", "-w e", "-l ", "-A ", "-N "} {
			Expect(out).NotTo(ContainSubstring(flag), flag)
		}
	})
})

var _ = Describe("SessionArgs geometry verification", func() {
	It("pins -par but omits -w e when the caller says it cannot be judged", func() {
		s := launch.SessionSpec{Queue: "all.q", PE: "make", Slots: 2,
			AllocationRule: "1", VerifyGeometry: false, Command: []string{"bash"}}
		joined := strings.Join(launch.SessionArgs(s), " ")
		Expect(joined).To(ContainSubstring("-par 1"), "the layout is still pinned")
		Expect(joined).NotTo(ContainSubstring("-w e"), "-w e would refuse a runnable job")
	})
})
