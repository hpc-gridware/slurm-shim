package layout_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// stephelperPath is the gexec-built path to the step-counter helper, shared
// across parallel nodes for the multi-process concurrency spec (REQ-TST-005).
var stephelperPath string

var _ = SynchronizedBeforeSuite(func() []byte {
	p, err := gexec.Build("github.com/hpc-gridware/slurm-shim/internal/layout/stephelper")
	Expect(err).NotTo(HaveOccurred())
	return []byte(p)
}, func(data []byte) {
	stephelperPath = string(data)
})

var _ = SynchronizedAfterSuite(func() {}, func() {
	gexec.CleanupBuildArtifacts()
})

func TestLayout(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Layout Suite")
}
