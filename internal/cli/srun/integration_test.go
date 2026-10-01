package srun_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
)

func lines(s *gexec.Session) []string {
	out := strings.TrimRight(string(s.Out.Contents()), "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

var _ = Describe("srun end-to-end over the local launcher", func() {
	It("runs N ranks across the allocation, one process each [REQ-RUN-020]", func() {
		tmp := twoByEight()
		sess := runSrun(tmp, "-n", "16", "sh", "-c", "echo $SLURM_PROCID")
		Eventually(sess, "30s").Should(gexec.Exit(0))

		got := lines(sess)
		sort.Strings(got)
		Expect(got).To(HaveLen(16))
		// Every global rank 0..15 produced exactly one line.
		want := make([]string, 16)
		for i := range want {
			want[i] = strconv.Itoa(i)
		}
		sort.Strings(want)
		Expect(got).To(Equal(want))
	})

	It("prefixes each line with the rank under -l [REQ-RUN-020]", func() {
		tmp := twoByEight()
		sess := runSrun(tmp, "-l", "-n", "4", "sh", "-c", "echo hi")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		got := lines(sess)
		sort.Strings(got)
		Expect(got).To(Equal([]string{"0: hi", "1: hi", "2: hi", "3: hi"}))
	})

	It("distributes ranks block-wise, one task per node with -N [REQ-RUN-025]", func() {
		tmp := twoByEight()
		sess := runSrun(tmp, "-N", "2", "sh", "-c", "echo $SLURM_NODEID")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		got := lines(sess)
		sort.Strings(got)
		Expect(got).To(Equal([]string{"0", "1"}))
	})

	It("propagates a failing rank's exit code under kill-on-bad-exit [REQ-RUN-022]", func() {
		tmp := twoByEight()
		sess := runSrun(tmp, "-n", "4", "sh", "-c", "exit 3")
		Eventually(sess, "30s").Should(gexec.Exit(3))
	})

	It("fails before launch when more tasks are requested than permitted [REQ-RUN-008]", func() {
		tmp := twoByEight()
		sess := runSrun(tmp, "-n", "100", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(1))
		Expect(sess.Err).To(gbytes.Say("More processors requested than permitted"))
	})

	It("shadows SLURM_NTASKS to the step geometry [REQ-ENV-041]", func() {
		tmp := twoByEight()
		sess := runSrun(tmp, "-n", "2", "sh", "-c", "echo ntasks=$SLURM_NTASKS")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(sess.Out).To(gbytes.Say("ntasks=2"))
	})

	It("rejects an unsupported --mpi value [REQ-RUN-004]", func() {
		tmp := twoByEight()
		sess := runSrun(tmp, "--mpi", "pmix", "-n", "1", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(1))
		Expect(sess.Err).To(gbytes.Say("mpi"))
	})

	It("reports a pre-exec failure as a rank failure [REQ-STP-006]", func() {
		tmp := twoByEight()
		sess := runSrun(tmp, "-n", "2", "--chdir", "/no/such/directory", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(1))
		Expect(sess.Err).To(gbytes.Say("failed to start"))
	})

	It("kills surviving ranks when one fails under -K [REQ-STP-004]", func() {
		tmp := twoByEight()
		// One rank exits 5 immediately; the others sleep 60s. kill-on-bad-exit
		// must SIGTERM the sleepers so srun returns promptly with the bad code,
		// not after the full sleep.
		start := time.Now()
		sess := runSrun(tmp, "-n", "8", "sh", "-c",
			`if [ "$SLURM_PROCID" = "0" ]; then exit 5; else sleep 60; fi`)
		Eventually(sess, "30s").Should(gexec.Exit(5))
		Expect(time.Since(start)).To(BeNumerically("<", 30*time.Second))
	})

	It("reports a rank killed by a signal as 128+S, not success, without -K [REQ-RUN-022]", func() {
		tmp := twoByEight()
		sess := runSrun(tmp, "--kill-on-bad-exit=0", "-n", "2", "sh", "-c",
			`if [ "$SLURM_PROCID" = "0" ]; then kill -TERM $$; fi`)
		Eventually(sess, "30s").Should(gexec.Exit(143))
	})

	It("SIGKILLs a rank that ignores SIGTERM over the control channel under -K [REQ-STP-004]", func() {
		// The escalation must reach the ranks through the stepper, never by
		// killing the stepper's launcher handle: under qrsh -inherit that makes
		// the pe task die by signal and qmaster deletes the whole job.
		tmp := twoByEight()
		pidDir := filepath.Join(tmp, "pids")
		Expect(os.Mkdir(pidDir, 0o700)).To(Succeed())
		// -K explicitly (not the config default); rank 0 waits so the other rank's
		// TERM trap is in place before the kill fan-out can reach it.
		sess := runSrun(tmp, "-K", "-N", "2", "sh", "-c",
			`if [ "$SLURM_PROCID" = "0" ]; then sleep 1; exit 5; fi; trap "" TERM; echo $$ > `+pidDir+`/$SLURM_PROCID; exec sleep 60`)
		// SIGTERM is ignored; the SIGKILL stage follows killEscalation (10s).
		Eventually(sess, "30s").Should(gexec.Exit(5))

		entries, err := os.ReadDir(pidDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		data, err := os.ReadFile(filepath.Join(pidDir, entries[0].Name()))
		Expect(err).NotTo(HaveOccurred())
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error { return syscall.Kill(pid, 0) }, "5s").Should(MatchError(syscall.ESRCH),
			"the rank that ignored SIGTERM must be gone")
	})

	It("escalates to SIGKILL when srun itself gets SIGTERM and the ranks ignore it [REQ-RUN-021]", func() {
		// scancel/timeout send srun SIGTERM. Forwarding it alone would leave srun
		// waiting forever on a rank that ignores it; the escalation bounds it.
		tmp := twoByEight()
		pidDir := filepath.Join(tmp, "pids")
		Expect(os.Mkdir(pidDir, 0o700)).To(Succeed())
		sess := runSrun(tmp, "-N", "2", "sh", "-c",
			`trap "" TERM; echo $$ > `+pidDir+`/$SLURM_PROCID; exec sleep 60`)
		Eventually(func() int {
			entries, _ := os.ReadDir(pidDir)
			return len(entries)
		}, "15s").Should(Equal(2))

		sess.Signal(syscall.SIGTERM)

		// SIGTERM is ignored; the SIGKILL stage follows killEscalation (10s).
		Eventually(sess, "30s").Should(gexec.Exit(137))
		entries, err := os.ReadDir(pidDir)
		Expect(err).NotTo(HaveOccurred())
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(pidDir, e.Name()))
			Expect(err).NotTo(HaveOccurred())
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error { return syscall.Kill(pid, 0) }, "5s").Should(MatchError(syscall.ESRCH))
		}
	})

	DescribeTable("survives a suspend longer than every liveness timeout [REQ-APX-005]",
		func(whole bool) {
			// GE suspend SIGSTOPs srun and the steppers (whole job), or only srun
			// (its queue instance alone, a debugger, Ctrl-Z). Either way the hosts'
			// kernels keep answering, so nothing may be declared lost or stopped.
			tmp := twoByEight()
			Expect(os.WriteFile(filepath.Join(tmp, "config.yaml"), []byte(
				"launcher: local\nping_interval: 1s\nping_deadline: 3s\norphan_grace: 2s\n"), 0o600)).To(Succeed())
			sess := runSrun(tmp, "-N", "2", "sh", "-c", "echo up; sleep 12")
			Eventually(func() int { return strings.Count(string(sess.Out.Contents()), "up") }, "15s").Should(Equal(2))

			stopped := []int{sess.Command.Process.Pid}
			if whole {
				stopped = append(stopped, descendants(sess.Command.Process.Pid)...)
				Expect(len(stopped)).To(BeNumerically(">=", 5), "srun, 2 steppers and 2 ranks")
			}
			for _, pid := range stopped {
				_ = syscall.Kill(pid, syscall.SIGSTOP)
			}
			time.Sleep(5 * time.Second) // longer than ping_deadline and orphan_grace
			for i := len(stopped) - 1; i >= 0; i-- {
				_ = syscall.Kill(stopped[i], syscall.SIGCONT)
			}

			Eventually(sess, "30s").Should(gexec.Exit(0))
			Expect(string(sess.Err.Contents())).NotTo(ContainSubstring("lost"))
		},
		Entry("the whole job", true),
		Entry("only srun", false),
	)

	It("writes per-rank output files from a %-pattern [REQ-RUN-003]", func() {
		tmp := twoByEight()
		pat := filepath.Join(tmp, "out.%t.log")
		sess := runSrun(tmp, "-n", "4", "-o", pat, "sh", "-c", "echo rank$SLURM_PROCID")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		// Streamed stdout is empty; each rank wrote its own file.
		Expect(lines(sess)).To(BeEmpty())
		for r := 0; r < 4; r++ {
			data, err := os.ReadFile(filepath.Join(tmp, "out."+strconv.Itoa(r)+".log"))
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(string(data))).To(Equal("rank" + strconv.Itoa(r)))
		}
	})
})

// descendants lists every process below pid (children first by depth), so a
// spec can suspend a whole step the way Grid Engine does.
func descendants(pid int) []int {
	out, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=").Output()
	Expect(err).NotTo(HaveOccurred())
	children := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		c, err1 := strconv.Atoi(f[0])
		p, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			children[p] = append(children[p], c)
		}
	}
	var all []int
	queue := []int{pid}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			all = append(all, c)
			queue = append(queue, c)
		}
	}
	return all
}
