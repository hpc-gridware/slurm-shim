package doctor_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/cli/doctor"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

func TestDoctor(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Doctor Suite")
}

var _ = Describe("CompatNotes [version matrix]", func() {
	It("is silent on the build that carries the accounting fix and anything newer", func() {
		Expect(doctor.CompatNotes(gedata.OCSBuild{Release: "9.1.5", Stamp: "250826-0734"})).To(BeEmpty())
		Expect(doctor.CompatNotes(gedata.OCSBuild{Release: "9.2.0", Stamp: "260101-0000"})).To(BeEmpty())
	})

	It("names every degradation on 9.0.x", func() {
		notes := doctor.CompatNotes(gedata.OCSBuild{Release: "9.0.10"})
		Expect(notes).To(HaveLen(3))
		Expect(notes[0]).To(ContainSubstring("sacct ExitCode"))
		Expect(notes[0]).To(ContainSubstring("250826-0734"), "the exact build that contains the fix")
		Expect(notes[1]).To(ContainSubstring("-par"))
		Expect(notes[2]).To(ContainSubstring("--x-spares"))
	})

	It("warns on an earlier 9.1.5 build that predates the fix", func() {
		Expect(doctor.CompatNotes(gedata.OCSBuild{Release: "9.1.5", Stamp: "250801-0000"})).NotTo(BeEmpty())
	})
})

// fakeQconf answers the qconf calls the lost-node checks make: the global
// configuration enables ENABLE_RESCHEDULE_SLAVE and lacks ENABLE_ADDGRP_KILL,
// which only exec host h1 sets in its local configuration. Everything else
// fails, as on a cluster the user cannot read.
const fakeQconf = `#!/bin/sh
case "$1" in
  -sel|-ss) printf 'h1\nh2\n' ;;
  -sconf)
    if [ $# -eq 1 ]; then
      printf 'qmaster_params ENABLE_RESCHEDULE_SLAVE=TRUE\nexecd_params NONE\n'
    elif [ "$2" = h1 ]; then
      printf '#h1:\nexecd_params ENABLE_ADDGRP_KILL=TRUE\n'
    else
      echo "configuration $2 not defined"; exit 1
    fi ;;
  *) echo "fake qconf: $*" >&2; exit 1 ;;
esac
`

var _ = Describe("Run [lost-node checks]", func() {
	BeforeEach(func() {
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, "qconf"), []byte(fakeQconf), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "qstat"), []byte("#!/bin/sh\nexit 1\n"), 0o755)).To(Succeed())
		cfg := filepath.Join(dir, "slurm-shim.yaml")
		Expect(os.WriteFile(cfg, []byte("partitions:\n  batch: {queue: all.q, pe: make, slots: \"1\"}\n"), 0o644)).To(Succeed())
		GinkgoT().Setenv("PATH", dir+":/usr/bin:/bin")
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfg)
		GinkgoT().Setenv("SGE_ROOT", "")
	})

	It("fails on ENABLE_RESCHEDULE_SLAVE and warns for the exec host without ENABLE_ADDGRP_KILL", func() {
		var stdout, stderr bytes.Buffer
		Expect(doctor.Run(nil, &stdout, &stderr)).To(Equal(1))
		out := stdout.String()
		Expect(out).To(MatchRegexp(`(?m)^FAIL  qmaster_params sets ENABLE_RESCHEDULE_SLAVE`))
		Expect(out).To(MatchRegexp(`(?m)^WARN  execd_params lacks ENABLE_ADDGRP_KILL=TRUE on exec host\(s\) h2:`),
			"h1 sets it in its local configuration")
	})
})
