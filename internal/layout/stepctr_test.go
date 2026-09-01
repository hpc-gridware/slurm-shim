package layout_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

var _ = Describe("Step counter [REQ-LCY-003]", func() {
	var ctr string

	BeforeEach(func() {
		ctr = filepath.Join(GinkgoT().TempDir(), layout.StepCtrFile)
		Expect(layout.InitStepCounter(ctr)).To(Succeed())
	})

	It("issues 0 first and increments densely", func() {
		for want := 0; want < 5; want++ {
			got, err := layout.NextStep(ctr)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		}
	})

	It("works without an explicit init (defaults last id to -1)", func() {
		fresh := filepath.Join(GinkgoT().TempDir(), "stepctr")
		got, err := layout.NextStep(fresh)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(0))
	})

	It("issues dense unique ids under 32 concurrent goroutines [REQ-TST-005]", func() {
		const n = 32
		var wg sync.WaitGroup
		ids := make([]int, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ids[i], errs[i] = layout.NextStep(ctr)
			}(i)
		}
		wg.Wait()

		for _, err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(distinctSorted(ids)).To(Equal(seq(n)))
	})

	It("issues dense unique ids across 16 concurrent processes [REQ-TST-005]", func() {
		const n = 16
		sessions := make([]*gexec.Session, n)
		for i := 0; i < n; i++ {
			cmd := exec.Command(stephelperPath, ctr)
			sess, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())
			sessions[i] = sess
		}
		ids := make([]int, n)
		for i, sess := range sessions {
			Eventually(sess, "10s").Should(gexec.Exit(0))
			ids[i] = mustParse(sess.Out)
		}
		Expect(distinctSorted(ids)).To(Equal(seq(n)))
	})
})

func mustParse(out *gbytes.Buffer) int {
	v, err := strconv.Atoi(strings.TrimSpace(string(out.Contents())))
	Expect(err).NotTo(HaveOccurred())
	return v
}

func distinctSorted(in []int) []int {
	seen := map[int]struct{}{}
	for _, v := range in {
		seen[v] = struct{}{}
	}
	out := make([]int, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

var _ = Describe("layout.PeekStep [REQ-LCY-003]", func() {
	var path string

	BeforeEach(func() {
		path = filepath.Join(GinkgoT().TempDir(), layout.StepCtrFile)
	})

	It("reports 0 for a freshly initialized counter, without consuming it", func() {
		Expect(layout.InitStepCounter(path)).To(Succeed())

		n, err := layout.PeekStep(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))

		// Peeking twice must not advance, and the next real issue must match.
		again, err := layout.PeekStep(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(0))

		issued, err := layout.NextStep(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(issued).To(Equal(0))
	})

	// The property the dry run depends on: after steps are consumed, the peek
	// reports the id the NEXT real srun gets. A stuck-at-zero implementation
	// passes the fresh-counter case above but fails this one.
	It("agrees with the next issue after steps have been consumed", func() {
		Expect(layout.InitStepCounter(path)).To(Succeed())
		for i := 0; i < 3; i++ {
			_, err := layout.NextStep(path)
			Expect(err).NotTo(HaveOccurred())
		}

		n, err := layout.PeekStep(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(3))

		issued, err := layout.NextStep(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(issued).To(Equal(3))
	})

	It("treats a missing or empty counter as the first step", func() {
		n, err := layout.PeekStep(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))

		Expect(os.WriteFile(path, nil, 0o600)).To(Succeed())
		n, err = layout.PeekStep(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))
	})

	// Diverging here is what let a dry run report "step id 0" for a job whose
	// every real srun would exit 1.
	It("reports a corrupt counter as an error, exactly as layout.NextStep does", func() {
		Expect(os.WriteFile(path, []byte("garbage\n"), 0o600)).To(Succeed())

		_, peekErr := layout.PeekStep(path)
		Expect(peekErr).To(HaveOccurred())
		Expect(peekErr.Error()).To(ContainSubstring("corrupt step counter"))

		_, nextErr := layout.NextStep(path)
		Expect(nextErr).To(HaveOccurred())
		Expect(nextErr.Error()).To(ContainSubstring("corrupt step counter"))
	})
})
