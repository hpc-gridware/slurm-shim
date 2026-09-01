package srun_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// dryEnv is the switch under test.
var dryEnv = []string{"SLURM_SHIM_DRY_RUN=1"}

// errOf is the report stream: srun's stdout belongs to the ranks, so everything
// the dry run says goes to stderr (REQ-LOG-003).
func errOf(s *gexec.Session) string { return string(s.Err.Contents()) }

var _ = Describe("srun dry run [REQ-DRY-002] [REQ-DRY-003] [REQ-DRY-004]", func() {
	It("reports the step and launches nothing", func() {
		tmp := twoByEight()
		// A command that would be unmistakable in the output if it ever ran.
		sess := runSrunEnv(tmp, dryEnv, "-n", "4", "sh", "-c", "echo LAUNCHED")
		Eventually(sess, "30s").Should(gexec.Exit(0))

		Expect(errOf(sess)).To(ContainSubstring("dry run"))
		Expect(errOf(sess)).To(ContainSubstring("would launch:"))
		Expect(string(sess.Out.Contents())).To(BeEmpty(),
			"stdout belongs to the ranks; a caller capturing it must get nothing")
	})

	It("does not consume a step id, so the next real step still gets 0", func() {
		tmp := twoByEight()
		ctr := filepath.Join(tmp, layout.StateDir, layout.StepCtrFile)

		sess := runSrunEnv(tmp, dryEnv, "-n", "1", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(errOf(sess)).To(MatchRegexp(`step id\s+0`))

		data, err := os.ReadFile(ctr)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(data))).To(Equal("-1"), "the counter must be untouched")

		// The step the dry run described is the one a real srun now creates.
		real := runSrun(tmp, "-n", "1", "sh", "-c", "echo $SLURM_STEP_ID")
		Eventually(real, "30s").Should(gexec.Exit(0))
		Expect(lines(real)).To(Equal([]string{"0"}))

		// And after a real step is consumed, the peek advances -- which a
		// stuck-at-zero implementation would fail.
		after := runSrunEnv(tmp, dryEnv, "-n", "1", "hostname")
		Eventually(after, "30s").Should(gexec.Exit(0))
		Expect(errOf(after)).To(MatchRegexp(`step id\s+1`))
	})

	It("refuses to report a step id from a corrupt counter", func() {
		tmp := twoByEight()
		ctr := filepath.Join(tmp, layout.StateDir, layout.StepCtrFile)
		Expect(os.WriteFile(ctr, []byte("garbage\n"), 0o600)).To(Succeed())

		sess := runSrunEnv(tmp, dryEnv, "-n", "1", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(1))
		Expect(errOf(sess)).To(ContainSubstring("corrupt step counter"))
	})

	It("reports every rank's placement and per-rank environment", func() {
		tmp := twoByEight()
		sess := runSrunEnv(tmp, dryEnv, "-N", "2", "--ntasks-per-node=2", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))

		out := errOf(sess)
		Expect(out).To(ContainSubstring("rank 0 on node001"))
		Expect(out).To(ContainSubstring("rank 3 on node002"))
		// The per-rank deltas are the whole point: rank 3 is local 1 on node 1.
		Expect(out).To(MatchRegexp(`(?s)rank 3 \(node002\) adds:.*SLURM_PROCID=3`))
		Expect(out).To(MatchRegexp(`(?s)rank 3 \(node002\) adds:.*SLURM_LOCALID=1`))
		// SLURMD_NODENAME is added by the stepper, not the planner, so a report
		// built from the rank spec alone silently omits it.
		Expect(out).To(MatchRegexp(`(?s)rank 3 \(node002\) adds:.*SLURMD_NODENAME=node002`))
		Expect(out).To(ContainSubstring("SLURM_NTASKS=4"))
	})

	It("reports the step-scoped output paths a rank would write", func() {
		tmp := twoByEight()
		sess := runSrunEnv(tmp, dryEnv, "-n", "2", "-o", "log-%t.out", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))

		out := errOf(sess)
		Expect(out).To(ContainSubstring("log-0.out"))
		Expect(out).To(ContainSubstring("log-1.out"))
		// Reporting a path must not create it.
		_, err := os.Stat(filepath.Join(tmp, "log-0.out"))
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("renders the qrsh launch line without a real token [REQ-CHN-003, SI-51]", func() {
		tmp := twoByEight()
		// The qrsh launcher is what a real allocation uses, and its argv is the one
		// that carries the per-step token.
		Expect(os.WriteFile(filepath.Join(tmp, "config.yaml"),
			[]byte("launcher: qrsh-inherit\n"), 0o600)).To(Succeed())

		sess := runSrunEnv(tmp, dryEnv, "-N", "2", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))

		out := errOf(sess)
		Expect(out).To(ContainSubstring("qrsh -inherit -nostdin -noshell"))
		Expect(out).To(ContainSubstring("SLURM_SHIM_TOKEN=<per-step token>"))
		Expect(out).NotTo(MatchRegexp(`[0-9a-f]{64}`))
	})

	// The report must never carry the shim's own control namespace: the token is
	// the sole authenticator of the control channel.
	It("never prints SLURM_SHIM_* variables from the environment", func() {
		tmp := twoByEight()
		token := strings.Repeat("ab", 32)
		sess := runSrunEnv(tmp, append(dryEnv,
			"SLURM_SHIM_TOKEN="+token,
			"SLURM_SHIM_TASK_POLICY=slot",
		), "-n", "1", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))

		out := errOf(sess)
		Expect(out).NotTo(ContainSubstring(token))
		Expect(out).NotTo(ContainSubstring("SLURM_SHIM_TOKEN="))
		Expect(out).NotTo(ContainSubstring("SLURM_SHIM_TASK_POLICY="))
		Expect(out).NotTo(ContainSubstring("SLURM_SHIM_DRY_RUN="))
		Expect(out).NotTo(ContainSubstring("SLURM_SHIM_CONFIG="))
	})

	It("reports a launcher it cannot build as fatal, with no launch plan", func() {
		tmp := twoByEight()
		Expect(os.WriteFile(filepath.Join(tmp, "config.yaml"),
			[]byte("launcher: ssh\n"), 0o600)).To(Succeed())

		sess := runSrunEnv(tmp, dryEnv, "-N", "2", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(8))

		out := errOf(sess)
		Expect(out).To(ContainSubstring("ERROR"))
		Expect(out).NotTo(ContainSubstring("would launch:"),
			"a plan must not be printed for a launcher that does not exist")
	})

	It("is reachable through --test-only with no environment variable", func() {
		tmp := twoByEight()
		sess := runSrunEnv(tmp, nil, "--test-only", "-n", "1", "sh", "-c", "echo LAUNCHED")
		Eventually(sess, "30s").Should(gexec.Exit(0))

		Expect(errOf(sess)).To(ContainSubstring("dry run"))
		Expect(string(sess.Out.Contents())).NotTo(ContainSubstring("LAUNCHED"))
	})

	It("is off for the conventional false values", func() {
		tmp := twoByEight()
		for _, v := range []string{"0", "no", "n", "off", "false", "none", "disabled"} {
			sess := runSrunEnv(tmp, []string{"SLURM_SHIM_DRY_RUN=" + v},
				"-n", "1", "sh", "-c", "echo ran")
			Eventually(sess, "30s").Should(gexec.Exit(0))
			Expect(lines(sess)).To(Equal([]string{"ran"}), "value %q must not enable dry run", v)
		}
	})
})
