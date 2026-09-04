package launch_test

import (
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The starter locates the environment hook relative to its own path, so a site
// installing under a prefix other than /opt/slurm-shim cannot end up with a
// starter reading the hook from a different tree. A hook it cannot read is a
// broken install rather than a host without the shim -- Grid Engine had to exec
// the starter out of the same tree to get here -- so it is reported instead of
// silently running every job in the queue with no SLURM_* environment.
var _ = Describe("starter hook resolution [REQ-FAB-010]", func() {
	var (
		prefix  string
		starter string
		job     string
	)

	// install lays down a shim tree at an arbitrary prefix: bin/ holds a copy of
	// the real starter, etc/ optionally holds a hook that marks having been run.
	install := func(withHook bool) {
		prefix = GinkgoT().TempDir()
		Expect(os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(prefix, "etc"), 0o755)).To(Succeed())

		wd, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred())
		src, err := os.ReadFile(filepath.Join(wd, "..", "..", "docs", "install", "slurm-shim-starter.sh"))
		Expect(err).NotTo(HaveOccurred())
		starter = filepath.Join(prefix, "bin", "slurm-shim-starter")
		Expect(os.WriteFile(starter, src, 0o755)).To(Succeed())

		if withHook {
			hook := filepath.Join(prefix, "etc", "slurm-shim-source-hook.sh")
			// The real hook emits export lines; a bare assignment would not survive exec.
			Expect(os.WriteFile(hook, []byte("export SLURM_SHIM_HOOK_RAN=yes\n"), 0o644)).To(Succeed())
		}
		job = filepath.Join(prefix, "job.sh")
		Expect(os.WriteFile(job, []byte("#!/bin/sh\necho JOB-RAN hook=${SLURM_SHIM_HOOK_RAN:-no}\n"), 0o755)).To(Succeed())
	}

	run := func(policy string) (string, int) {
		cmd := exec.Command("/bin/sh", starter, job)
		cmd.Env = append(os.Environ(), "SLURM_SHIM_HOOK_MISSING_ENV="+policy)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			Expect(err).NotTo(HaveOccurred(), string(out))
		}
		return string(out), code
	}

	It("sources the hook from its own prefix, not a hard-coded one", func() {
		install(true)
		out, code := run("continue")
		Expect(code).To(Equal(0))
		Expect(out).To(ContainSubstring("JOB-RAN hook=yes"),
			"the hook beside this starter must be the one that ran")
	})

	It("reports a missing hook instead of running the job silently unconfigured", func() {
		install(false)
		out, code := run("continue")
		Expect(out).To(ContainSubstring("cannot read"))
		Expect(out).To(ContainSubstring("no SLURM_* environment"))
		Expect(out).To(ContainSubstring(prefix), "the diagnostic must name the path it looked at")
		Expect(out).To(ContainSubstring("JOB-RAN hook=no"), "default policy still runs the job")
		Expect(code).To(Equal(0))
	})

	It("aborts on a missing hook under SLURM_SHIM_HOOK_MISSING_ENV=abort", func() {
		install(false)
		out, code := run("abort")
		Expect(code).To(Equal(1))
		Expect(out).To(ContainSubstring("aborting job"))
		Expect(out).NotTo(ContainSubstring("JOB-RAN"), "the job must not run")
	})

	It("still passes a stepper launch through with no hook present", func() {
		install(false)
		stub := filepath.Join(prefix, "bin", "slurm-shim")
		Expect(os.WriteFile(stub, []byte("#!/bin/sh\necho STEPPER-RAN\n"), 0o755)).To(Succeed())
		cmd := exec.Command("/bin/sh", starter, stub, "stepper", "--envelope", "X")
		cmd.Env = append(os.Environ(), "SLURM_SHIM_HOOK_MISSING_ENV=abort")
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(out).To(ContainSubstring("STEPPER-RAN"),
			"steppers short-circuit before the hook, so a missing hook cannot kill a step")
	})
})
