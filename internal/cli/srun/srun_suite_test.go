package srun_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// shimBin is the gexec-built shim binary, shared across parallel nodes. srun
// dispatches steppers via os.Executable(), so the whole stack runs from this one
// binary over a loopback control channel (D-6).
var shimBin string

var _ = SynchronizedBeforeSuite(func() []byte {
	p, err := gexec.Build("github.com/hpc-gridware/slurm-shim/cmd/slurm-shim")
	Expect(err).NotTo(HaveOccurred())
	return []byte(p)
}, func(data []byte) {
	shimBin = string(data)
})

var _ = SynchronizedAfterSuite(func() {}, func() {
	gexec.CleanupBuildArtifacts()
})

func TestSrun(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Srun Suite")
}

// writeAlloc writes a fabricated layout and step counter into a fresh TMPDIR and
// returns that TMPDIR. The named hosts all run locally under the LocalLauncher.
func writeAlloc(nodes []layout.Node, perNode []int) string {
	tmp := GinkgoT().TempDir()
	dir := filepath.Join(tmp, layout.StateDir)
	ntasks := 0
	for _, c := range perNode {
		ntasks += c
	}
	lay := &layout.Layout{
		SchemaVersion: layout.SchemaVersion,
		ShimVersion:   "test",
		Job:           layout.Job{JobID: 4711, Name: "test"},
		Nodes:         nodes,
		Tasks:         layout.Tasks{NTasks: ntasks, CPUsPerTask: 1, PerNode: perNode},
		Launcher:      "qrsh-inherit",
	}
	Expect(layout.Write(dir, lay)).To(Succeed())
	Expect(layout.InitStepCounter(filepath.Join(dir, layout.StepCtrFile))).To(Succeed())
	return tmp
}

func twoByEight() string {
	return writeAlloc([]layout.Node{
		{Index: 0, Host: "node001", Slots: 8, IsMaster: true},
		{Index: 1, Host: "node002", Slots: 8},
	}, []int{8, 8})
}

// runSrun runs `shim srun <args>` with the given TMPDIR and returns the session.
// The suite has no GE cluster, so every host launches locally: a config with
// launcher: local keeps all other defaults (Parse overlays onto Default()).
func runSrun(tmp string, args ...string) *gexec.Session {
	cfgPath := filepath.Join(tmp, "config.yaml")
	Expect(os.WriteFile(cfgPath, []byte("launcher: local\n"), 0o600)).To(Succeed())
	cmd := exec.Command(shimBin, append([]string{"srun"}, args...)...)
	cmd.Env = []string{
		"TMPDIR=" + tmp,
		"SLURM_SHIM_CONFIG=" + cfgPath,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
	}
	sess, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return sess
}
