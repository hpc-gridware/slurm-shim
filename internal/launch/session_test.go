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
	base := launch.SessionSpec{Queue: "all.q", PE: "make", Slots: 2, Command: []string{"bash"}}

	joined := func(s launch.SessionSpec) string { return strings.Join(launch.SessionArgs(s), " ") }

	It("always requests a queued, pty session in the invocation dir", func() {
		// -now no (queue and wait), -pty y (force a terminal), -cwd (SLURM's
		// working dir) -- all verified required on OCS 9.1.5.
		Expect(joined(base)).To(HavePrefix("-now no -pty y -cwd -q all.q -pe make 2"))
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
