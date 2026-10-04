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

var _ = Describe("Run [PE slots]", func() {
	// The fake cluster is the captured OCS 9.1.6 one: 3 x 14 = 42 slots in
	// all.q (qstat -f). Its make PE is served with slots 16, so a 17-slot job on
	// the batch partition could never start.
	BeforeEach(func() {
		fixtures, err := filepath.Abs("../../../test/e2e/fixtures/9.1.6")
		Expect(err).NotTo(HaveOccurred())
		dir := GinkgoT().TempDir()
		qconf := `#!/bin/sh
case "$1" in
  -sp) sed 's/^slots .*/slots              16/' ` + fixtures + `/qconf-sp-make.txt ;;
  -sq) printf 'qname all.q\nhostlist @allhosts\npe_list make\nslots 14\nshell_start_mode unix_behavior\nstarter_method NONE\n' ;;
  *) echo "fake qconf: $*" >&2; exit 1 ;;
esac
`
		Expect(os.WriteFile(filepath.Join(dir, "qconf"), []byte(qconf), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "qstat"), []byte("#!/bin/sh\ncat "+fixtures+"/qstat-f.txt\n"), 0o755)).To(Succeed())
		cfg := filepath.Join(dir, "slurm-shim.yaml")
		Expect(os.WriteFile(cfg, []byte("partitions:\n  batch: {queue: all.q, pe: make, slots: \"1\"}\n"), 0o644)).To(Succeed())
		GinkgoT().Setenv("PATH", dir+":/usr/bin:/bin")
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfg)
		GinkgoT().Setenv("SGE_ROOT", "")
	})

	It("warns that the partition's PE caps below its queue's slots, with the fix", func() {
		var stdout, stderr bytes.Buffer
		doctor.Run(nil, &stdout, &stderr)
		Expect(stdout.String()).To(MatchRegexp(
			`(?m)^WARN  pe make caps every job using it, together, at 16 slots; queue\(s\) all.q have 42: .*qconf -mattr pe slots 9999999 make`))
	})
})

var _ = Describe("Run [hosts that carry two queues]", func() {
	// The captured 9.1.6 cluster with a slurm.q next to all.q on every host, as
	// a default install creates it; the exec hosts' slots limit is removed.
	BeforeEach(func() {
		fixtures, err := filepath.Abs("../../../test/e2e/fixtures/9.1.6")
		Expect(err).NotTo(HaveOccurred())
		dir := GinkgoT().TempDir()
		qconf := `#!/bin/sh
case "$1" in
  -sp) cat ` + fixtures + `/qconf-sp-make.txt ;;
  -sq) printf 'qname %s\nhostlist @allhosts\npe_list make\nslots 14\nshell_start_mode unix_behavior\nstarter_method NONE\n' "$2" ;;
  -se) sed -e "s/^hostname .*/hostname $2/" -e 's/slots=14,//' ` + fixtures + `/qconf-se-worker1.txt ;;
  *) echo "fake qconf: $*" >&2; exit 1 ;;
esac
`
		Expect(os.WriteFile(filepath.Join(dir, "qconf"), []byte(qconf), 0o755)).To(Succeed())
		qstat := "#!/bin/sh\nsed -n 'p; s/^all\\.q@/slurm.q@/p' " + fixtures + "/qstat-f.txt\n"
		Expect(os.WriteFile(filepath.Join(dir, "qstat"), []byte(qstat), 0o755)).To(Succeed())
		cfg := filepath.Join(dir, "slurm-shim.yaml")
		Expect(os.WriteFile(cfg, []byte("partitions:\n  slurm: {queue: slurm.q, pe: make, slots: per-task}\n"), 0o644)).To(Succeed())
		GinkgoT().Setenv("PATH", dir+":/usr/bin:/bin")
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfg)
		GinkgoT().Setenv("SGE_ROOT", "")
	})

	It("warns that a host without a slots limit can run more jobs than it has cores", func() {
		var stdout, stderr bytes.Buffer
		doctor.Run(nil, &stdout, &stderr)
		Expect(stdout.String()).To(MatchRegexp(
			`(?m)^WARN  exec host ocs-master carries queues all.q,slurm.q and has no slots limit: .*complex_values slots=<cores> ocs-master`))
	})
})
