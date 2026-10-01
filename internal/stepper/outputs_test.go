package stepper

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("rank output files", func() {
	var path string
	BeforeEach(func() {
		path = filepath.Join(GinkgoT().TempDir(), "rank.out")
		Expect(os.WriteFile(path, []byte("before\n"), 0o644)).To(Succeed())
	})
	write := func(appendOutput bool, text string) {
		f, err := openOutput(path, appendOutput)
		Expect(err).NotTo(HaveOccurred())
		_, err = f.WriteString(text)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Close()).To(Succeed())
	}

	It("truncates for a new step", func() {
		write(false, "after\n")
		Expect(os.ReadFile(path)).To(Equal([]byte("after\n")))
	})

	It("continues what the lost node wrote after a hot-spare relaunch", func() {
		write(true, "after\n")
		Expect(os.ReadFile(path)).To(Equal([]byte("before\nafter\n")))
	})

	It("never lets a stale writer overwrite the replacement's lines", func() {
		stale, err := openOutput(path, false) // the stepper on the lost host
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = stale.Close() }()
		write(true, "replacement\n")
		_, err = stale.WriteString("stale\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(os.ReadFile(path)).To(Equal([]byte("replacement\nstale\n")))
	})
})
