package installcmd

import (
	"bytes"
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
