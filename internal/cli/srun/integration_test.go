package srun_test

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
