package launch_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/launch"
)

// The queue starter_method sits in front of every job in the queue, including
// srun's own qrsh -inherit stepper launches, which it must pass straight through
// rather than gate on the environment hook. That recognition is a shell pattern
// in docs/install/slurm-shim-starter.sh, outside Go's reach: nothing else fails
// if the two encodings of the stepper argv drift apart, and the symptom would be
// silent (every multi-node step running with no SLURM_* environment, or dying
// under a site abort policy).
//
// These specs feed the starter the argv buildQrshArgs really produces, via the
// exported QrshPreview, so a change to either side breaks the build.
var _ = Describe("starter_method / qrsh argv contract [REQ-RUN-009]", func() {
	var (
		starter string
		tmp     string
		stub    string
	)

	BeforeEach(func() {
		wd, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred())
		starter = filepath.Join(wd, "..", "..", "docs", "install", "slurm-shim-starter.sh")
		Expect(starter).To(BeAnExistingFile())

		tmp = GinkgoT().TempDir()
		// The stepper is launched as the shim binary itself, so the stub has to
		// sit at a path with that basename for the starter to recognize it.
		stub = filepath.Join(tmp, "slurm-shim")
		Expect(os.WriteFile(stub, []byte("#!/bin/sh\necho STEPPER-RAN \"$@\"\n"), 0o755)).To(Succeed())
	})

	// runStarter executes the real starter with args, in a shell-start mode that
	// makes the two outcomes distinguishable: when the starter passes argv
	// through it execs args[0] (the stub); when it does not, it falls through to
	// the shell dispatch and execs /bin/echo instead.
	runStarter := func(args ...string) string {
		cmd := exec.Command("/bin/sh", append([]string{starter}, args...)...)
		cmd.Env = append(os.Environ(),
			"SGE_STARTER_SHELL_START_MODE=posix_compliant",
			"SGE_STARTER_SHELL_PATH=/bin/echo",
			"SGE_STARTER_USE_LOGIN_SHELL=false",
		)
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		return string(out)
	}

	// remoteCommand is the tail of the qrsh argv: everything the remote host is
	// asked to execute, which is exactly what the starter receives in "$@".
	remoteCommand := func(self, host string) []string {
		argv := launch.QrshPreview(self, host, "ENVELOPE", "TOKEN")
		for i, a := range argv {
			if a == host {
				return argv[i+1:]
			}
		}
		Fail("host not found in qrsh argv: " + strings.Join(argv, " "))
		return nil
	}

	It("passes the launcher's real stepper argv straight through", func() {
		cmd := remoteCommand(stub, "ocs-worker1")
		Expect(cmd[0]).To(Equal(stub), "argv[0] is the shim binary")
		Expect(cmd[1]).To(Equal("stepper"), "argv[1] is the subcommand the starter matches on")
		Expect(runStarter(cmd...)).To(HavePrefix("STEPPER-RAN"),
			"the starter must exec the stepper without consulting the hook")
	})

	It("does not pass an ordinary job script through", func() {
		job := filepath.Join(tmp, "job.sh")
		Expect(os.WriteFile(job, []byte("#!/bin/sh\necho STEPPER-RAN\n"), 0o755)).To(Succeed())
		Expect(runStarter(job)).NotTo(ContainSubstring("STEPPER-RAN"),
			"a job script must reach the shell dispatch, not the stepper short-circuit")
	})

	// Regression: the match used to run against a flattened "$1 $2", so a space
	// inside an argument was indistinguishable from the argv separator and a job
	// argument was enough to skip the hook.
	It("does not let a job argument forge the stepper match", func() {
		job := filepath.Join(tmp, "job.sh")
		Expect(os.WriteFile(job, []byte("#!/bin/sh\necho STEPPER-RAN\n"), 0o755)).To(Succeed())
		Expect(runStarter(job, "--data /mnt/slurm-shim-stepper x")).NotTo(ContainSubstring("STEPPER-RAN"),
			"an argument containing a stepper-shaped path must not short-circuit the hook")
	})

	It("does not short-circuit the shim binary without the stepper subcommand", func() {
		Expect(runStarter(stub, "rank-exec")).NotTo(ContainSubstring("STEPPER-RAN"),
			"only the stepper subcommand is a shim-internal launch")
	})
})
