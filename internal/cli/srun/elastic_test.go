package srun_test

import (
	"encoding/base64"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// sparesAlloc is twoByEight plus hot spares; every host runs locally under the
// LocalLauncher, so a "relaunch on the spare" is a real second stepper process.
func sparesAlloc(spares ...string) string {
	tmp := twoByEight()
	dir := filepath.Join(tmp, layout.StateDir)
	Expect(layout.Update(dir, func(l *layout.Layout) error {
		for i, h := range spares {
			l.Spares = append(l.Spares, layout.Node{Index: i, Host: h, Slots: 8})
		}
		return nil
	})).To(Succeed())
	return tmp
}

// stepperFor finds the stepper process serving host among srun's descendants,
// by the host in its envelope (base64 JSON, the last argv word).
func stepperFor(srunPid int, host string) int {
	for _, pid := range descendants(srunPid) {
		out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || !strings.Contains(string(out), "stepper --envelope") {
			continue
		}
		fields := strings.Fields(string(out))
		env, err := base64.StdEncoding.DecodeString(fields[len(fields)-1])
		if err == nil && strings.Contains(string(env), `"host":"`+host+`"`) {
			return pid
		}
	}
	return 0
}

func readLayout(tmp string) *layout.Layout {
	l, err := layout.Read(filepath.Join(tmp, layout.StateDir, layout.LayoutFile))
	Expect(err).NotTo(HaveOccurred())
	return l
}

var _ = Describe("hot spares: elastic srun [sbatch --x-spares]", func() {
	// Each rank says where it runs, then waits long enough for the test to kill
	// its node's stepper; a relaunched rank finishes quickly.
	const script = `echo "rank $SLURM_PROCID on $SLURMD_NODENAME"; ` +
		`if [ -e "$TMPDIR/relaunched.$SLURM_PROCID" ]; then exit 0; fi; ` +
		`touch "$TMPDIR/relaunched.$SLURM_PROCID"; sleep 30`

	killStepper := func(sess *gexec.Session, host string) {
		var pid int
		Eventually(func() int {
			pid = stepperFor(sess.Command.Process.Pid, host)
			return pid
		}, "15s").ShouldNot(BeZero())
		Expect(syscall.Kill(pid, syscall.SIGKILL)).To(Succeed())
	}

	// Killing a stepper is an early end, not a lost host (that needs kernel
	// liveness, which loopback cannot lose): it gets one spare per step and no
	// drain. test/e2e/33_spares.sh covers the lost-host path.
	It("relaunches an ended stepper's tasks on a spare and the step succeeds", func() {
		tmp := sparesAlloc("node003")
		sess := runSrunEnv(tmp, []string{"TMPDIR=" + tmp}, "--x-elastic=on", "-N", "2", "sh", "-c", script)
		Eventually(sess.Out, "15s").Should(gbytes.Say("rank 1 on node002"))

		killStepper(sess, "node002")

		Eventually(sess, "60s").Should(gexec.Exit(0))
		Expect(string(sess.Out.Contents())).To(ContainSubstring("rank 1 on node003"))
		Expect(string(sess.Err.Contents())).To(ContainSubstring(
			"stepper on node002 ended before its tasks reported; tasks 1 relaunched on node003 (1/1 spares used)"))

		l := readLayout(tmp)
		Expect(l.Nodes[1].Host).To(Equal("node003"), "later steps run on the replacement")
		Expect(l.Lost).To(Equal([]string{"node002"}))
		Expect(l.Spares).To(BeEmpty())
		Expect(l.Swaps).To(HaveLen(1))
	})

	It("uses one spare per step for steppers that end early, then fails the step", func() {
		tmp := sparesAlloc("node003", "node004")
		long := `echo "rank $SLURM_PROCID on $SLURMD_NODENAME"; sleep 30`
		sess := runSrunEnv(tmp, []string{"TMPDIR=" + tmp}, "--x-elastic=on", "-N", "2", "sh", "-c", long)
		Eventually(sess.Out, "15s").Should(gbytes.Say("rank 1 on node002"))

		killStepper(sess, "node002")
		Eventually(sess.Out, "30s").Should(gbytes.Say("rank 1 on node003"))
		killStepper(sess, "node003")

		Eventually(sess, "60s").Should(gexec.Exit(1))
		Expect(string(sess.Err.Contents())).To(ContainSubstring("stepper on node003 ended before its tasks reported again"))
		Expect(readLayout(tmp).Spares).To(HaveLen(1), "the second spare is kept for a real node loss")
	})

	It("never swaps with --x-elastic=off: a lost node fails the step as without spares", func() {
		tmp := sparesAlloc("node003")
		long := `echo "rank $SLURM_PROCID on $SLURMD_NODENAME"; sleep 30`
		sess := runSrunEnv(tmp, []string{"TMPDIR=" + tmp}, "--x-elastic=off", "-N", "2", "sh", "-c", long)
		Eventually(sess.Out, "15s").Should(gbytes.Say("rank 1 on node002"))

		killStepper(sess, "node002")

		Eventually(sess, "60s").Should(gexec.Exit(1))
		Expect(string(sess.Err.Contents())).NotTo(ContainSubstring("relaunched"))
		Expect(readLayout(tmp).Spares).To(HaveLen(1), "the spare is untouched")
	})

	It("does not treat a plain (non-torchrun) step as elastic by default", func() {
		tmp := sparesAlloc("node003")
		long := `echo "rank $SLURM_PROCID on $SLURMD_NODENAME"; sleep 30`
		sess := runSrunEnv(tmp, []string{"TMPDIR=" + tmp}, "-N", "2", "sh", "-c", long)
		Eventually(sess.Out, "15s").Should(gbytes.Say("rank 1 on node002"))

		killStepper(sess, "node002")

		Eventually(sess, "60s").Should(gexec.Exit(1))
		Expect(readLayout(tmp).Spares).To(HaveLen(1))
	})

	// [AC9] A swap is for a lost node, not a failing program: a rank that exits
	// non-zero on its own still ends the step through kill-on-bad-exit.
	It("does not swap when a rank exits non-zero on its own", func() {
		tmp := sparesAlloc("node003")
		failing := `if [ "$SLURM_PROCID" = 1 ]; then exit 3; fi; sleep 30`
		sess := runSrunEnv(tmp, []string{"TMPDIR=" + tmp}, "--x-elastic=on", "-K", "-N", "2", "sh", "-c", failing)

		Eventually(sess, "60s").Should(gexec.Exit(3))
		Expect(string(sess.Err.Contents())).NotTo(ContainSubstring("relaunched"))
		Expect(readLayout(tmp).Spares).To(HaveLen(1), "the spare is untouched")
	})
})
