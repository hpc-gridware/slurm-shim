package installcmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/install"
)

func TestInstallCmd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "InstallCmd Suite")
}

var _ = Describe("printPlan", func() {
	It("renders an addition to pe_list as +=, not as a replacement", func() {
		var b bytes.Buffer
		printPlan(&b, install.Plan{Changes: []install.Change{
			{Kind: install.ChangeAddToPEList, Object: "all.q", Attr: "pe_list", Old: "make smp", New: "slurm-shim"},
		}})
		Expect(b.String()).To(ContainSubstring("make smp += slurm-shim"))
		Expect(b.String()).NotTo(ContainSubstring("->"))
	})

	It("renders a refusal with its reason and an unset old value as NONE", func() {
		var b bytes.Buffer
		printPlan(&b, install.Plan{Changes: []install.Change{
			{Kind: install.ChangeRefused, Object: "all.q", Attr: "starter_method", Old: "/site/s.sh", New: "/x", Reason: "two starters cannot be chained"},
			{Kind: install.ChangeSetStarter, Object: "gpu.q", Attr: "starter_method", Old: "", New: "/x"},
		}})
		Expect(b.String()).To(ContainSubstring("REFUSED"))
		Expect(b.String()).To(ContainSubstring("two starters cannot be chained"))
		Expect(b.String()).To(ContainSubstring("NONE -> /x"))
	})
})

var _ = Describe("writeConfigAtomic", func() {
	var dir string
	BeforeEach(func() { dir = GinkgoT().TempDir() })

	It("creates a new config readable by the users who run jobs", func() {
		path := filepath.Join(dir, "config.yaml")
		Expect(writeConfigAtomic(path, []byte("a: 1\n"))).To(Succeed())
		fi, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o644)))
	})

	It("PRESERVES a restricted mode instead of widening it on upgrade", func() {
		// A site that chose 0600 must not have it silently reopened to the world
		// by an install. os.WriteFile's mode argument applies only on create, so
		// the old code happened to preserve this -- writing via a temp file does
		// not, unless it is carried over deliberately.
		path := filepath.Join(dir, "config.yaml")
		Expect(os.WriteFile(path, []byte("a: 1\n"), 0o600)).To(Succeed())
		Expect(writeConfigAtomic(path, []byte("a: 2\n"))).To(Succeed())
		fi, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})

	It("leaves no temp files behind", func() {
		path := filepath.Join(dir, "config.yaml")
		Expect(writeConfigAtomic(path, []byte("a: 1\n"))).To(Succeed())
		Expect(writeConfigAtomic(path, []byte("a: 2\n"))).To(Succeed())
		ents, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(ents).To(HaveLen(1))
		Expect(ents[0].Name()).To(Equal("config.yaml"))
	})

	It("writes the content it was given", func() {
		path := filepath.Join(dir, "config.yaml")
		Expect(writeConfigAtomic(path, []byte("kill_wait: 45s\n"))).To(Succeed())
		Expect(os.ReadFile(path)).To(Equal([]byte("kill_wait: 45s\n")))
	})
})
