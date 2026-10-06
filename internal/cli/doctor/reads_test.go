package doctor_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/cli/doctor"
)

// Each object doctor needs is asked for once per run: a repeated read is a
// qconf fork against qmaster, and the per-host ones grow with the cluster.
var _ = Describe("Run reads each cluster object once", func() {
	var dir, log string
	var hosts []string

	BeforeEach(func() {
		fixtures, err := filepath.Abs("../../../test/e2e/fixtures/9.1.6")
		Expect(err).NotTo(HaveOccurred())
		dir = GinkgoT().TempDir()
		log = filepath.Join(dir, "qconf.log")

		// The captured 9.1.6 cluster (3 hosts carrying all.q and slurm.q) plus
		// 47 more exec hosts; 2 of the 50 have a local configuration.
		hosts = []string{"ocs-master", "ocs-worker1", "ocs-worker2"}
		for i := 1; i <= 47; i++ {
			hosts = append(hosts, fmt.Sprintf("h%d", i))
		}
		qconf := `#!/bin/sh
echo "$*" >> ` + log + `
case "$1" in
  -sel|-ss) printf '` + strings.Join(hosts, `\n`) + `\n' ;;
  -sconfl) printf 'ocs-master\nh7\n' ;;
  -sconf)
    if [ $# -eq 1 ] || [ "$2" = global ]; then
      printf 'qmaster_params NONE\nexecd_params ENABLE_ADDGRP_KILL=TRUE\nrsh_daemon builtin\n'
    else
      printf '#%s:\nexecd_params KEEP_ACTIVE=TRUE\n' "$2"
    fi ;;
  -sp) sed "s/^pe_name .*/pe_name $2/" ` + fixtures + `/qconf-sp-make.txt ;;
  -sq) printf 'qname %s\nhostlist @allhosts\npe_list make smp\nslots 14\nshell_start_mode unix_behavior\nstarter_method NONE\n' "$2" ;;
  -se) sed -e "s/^hostname .*/hostname $2/" -e 's/slots=14,//' ` + fixtures + `/qconf-se-worker1.txt ;;
  *) echo "fake qconf: $*" >&2; exit 1 ;;
esac
`
		Expect(os.WriteFile(filepath.Join(dir, "qconf"), []byte(qconf), 0o755)).To(Succeed())
		qstat := "#!/bin/sh\nsed -n 'p; s/^all\\.q@/slurm.q@/p' " + fixtures + "/qstat-f.txt\n"
		Expect(os.WriteFile(filepath.Join(dir, "qstat"), []byte(qstat), 0o755)).To(Succeed())
		GinkgoT().Setenv("PATH", dir+":/usr/bin:/bin")
		GinkgoT().Setenv("SGE_ROOT", "")
	})

	runWith := func(cfg string) []string {
		path := filepath.Join(dir, "slurm-shim.yaml")
		Expect(os.WriteFile(path, []byte(cfg), 0o644)).To(Succeed())
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", path)
		var stdout, stderr bytes.Buffer
		doctor.Run(nil, &stdout, &stderr)
		data, err := os.ReadFile(log)
		Expect(err).NotTo(HaveOccurred())
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	count := func(calls []string, match func(string) bool) int {
		n := 0
		for _, c := range calls {
			if match(c) {
				n++
			}
		}
		return n
	}
	is := func(want string) func(string) bool { return func(c string) bool { return c == want } }
	prefix := func(p string) func(string) bool { return func(c string) bool { return strings.HasPrefix(c, p) } }

	partitions := "partitions:\n" +
		"  a: {queue: slurm.q, pe: make, slots: per-task}\n" +
		"  b: {queue: slurm.q, pe: make, slots: per-task}\n" +
		"  c: {queue: slurm.q, pe: smp, slots: per-task}\n" +
		"  d: {queue: slurm.q, pe: smp, slots: per-task}\n"

	It("reads a shared queue once, the global config once, and only the local configs that exist", func() {
		calls := runWith(partitions)
		Expect(count(calls, prefix("-sq "))).To(Equal(1), "four partitions on slurm.q, and the forks check reuses it")
		Expect(count(calls, prefix("-sp "))).To(Equal(2), "one read per PE")
		Expect(count(calls, func(c string) bool { return c == "-sconf" || c == "-sconf global" })).To(Equal(1))
		Expect(count(calls, is("-sconfl"))).To(Equal(1))
		Expect(count(calls, func(c string) bool {
			return strings.HasPrefix(c, "-sconf ") && c != "-sconf global"
		})).To(Equal(2), "50 exec hosts, 2 with a local configuration")
	})

	It("reads each exec host once even when the cgroup and slots checks both need it", func() {
		calls := runWith(partitions + "gpu: {isolation: cgroup}\n")
		for _, h := range hosts {
			Expect(count(calls, is("-se "+h))).To(Equal(1), h)
		}
	})
})

// todo 104: the PE -> queues bookkeeping in Run decides which queues judge a
// PE's daemon_forks_slaves FALSE. A queue reached only through a later
// partition of an already-read PE must still be judged.
var _ = Describe("Run [daemon_forks_slaves wiring]", func() {
	BeforeEach(func() {
		fixtures, err := filepath.Abs("../../../test/e2e/fixtures/9.1.6")
		Expect(err).NotTo(HaveOccurred())
		dir := GinkgoT().TempDir()
		qconf := `#!/bin/sh
case "$1" in
  -sp) cat ` + fixtures + `/qconf-sp-make.txt ;;
  -sq)
    printf 'qname %s\nhostlist @allhosts\npe_list make\nslots 14\nshell_start_mode unix_behavior\nstarter_method NONE\n' "$2"
    [ "$2" = big.q ] && printf 'h_vmem 4G\n'
    exit 0 ;;
  *) echo "fake qconf: $*" >&2; exit 1 ;;
esac
`
		Expect(os.WriteFile(filepath.Join(dir, "qconf"), []byte(qconf), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "qstat"), []byte("#!/bin/sh\nexit 1\n"), 0o755)).To(Succeed())
		cfg := filepath.Join(dir, "slurm-shim.yaml")
		Expect(os.WriteFile(cfg, []byte("partitions:\n"+
			"  a: {queue: small.q, pe: make, slots: \"1\"}\n"+
			"  b: {queue: big.q, pe: make, slots: \"1\"}\n"), 0o644)).To(Succeed())
		GinkgoT().Setenv("PATH", dir+":/usr/bin:/bin")
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfg)
		GinkgoT().Setenv("SGE_ROOT", "")
	})

	It("judges every queue offering the PE, and reports control_slaves once", func() {
		var stdout, stderr bytes.Buffer
		doctor.Run(nil, &stdout, &stderr)
		out := stdout.String()
		Expect(out).To(MatchRegexp(`(?m)^WARN  pe make: .*queue "big.q" sets h_vmem=4G`))
		Expect(out).NotTo(MatchRegexp(`(?m)^PASS  pe make daemon_forks_slaves FALSE`))
		Expect(strings.Count(out, "pe make control_slaves")).To(Equal(1))
	})
})

// todo 189: a failed qconf -sconfl is one fact, reported once -- not once per
// GPU host by the cgroup check.
var _ = Describe("Run [qconf -sconfl fails]", func() {
	var dir string

	setup := func(sconfl, sconfHost string) {
		fixtures, err := filepath.Abs("../../../test/e2e/fixtures/9.1.6")
		Expect(err).NotTo(HaveOccurred())
		dir = GinkgoT().TempDir()
		qconf := `#!/bin/sh
case "$1" in
  -sel|-ss) printf 'n1\nn2\nn3\nn4\nn5\n' ;;
  -sconfl) ` + sconfl + ` ;;
  -sconf)
    if [ $# -eq 1 ] || [ "$2" = global ]; then
      printf 'qmaster_params NONE\nexecd_params ENABLE_ADDGRP_KILL=TRUE\nrsh_daemon builtin\n'
    else
      ` + sconfHost + `
    fi ;;
  -se) sed -e "s/^hostname .*/hostname $2/" ` + fixtures + `/qconf-se-worker1.txt ;;
  *) echo "fake qconf: $*" >&2; exit 1 ;;
esac
`
		Expect(os.WriteFile(filepath.Join(dir, "qconf"), []byte(qconf), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "qstat"), []byte("#!/bin/sh\nexit 1\n"), 0o755)).To(Succeed())
		cfg := filepath.Join(dir, "slurm-shim.yaml")
		Expect(os.WriteFile(cfg, []byte("partitions:\n  a: {queue: all.q, pe: make, slots: \"1\"}\n"+
			"gpu: {isolation: cgroup}\n"), 0o644)).To(Succeed())
		GinkgoT().Setenv("PATH", dir+":/usr/bin:/bin")
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", cfg)
		GinkgoT().Setenv("SGE_ROOT", "")
	}
	run := func() string {
		var stdout, stderr bytes.Buffer
		doctor.Run(nil, &stdout, &stderr)
		return stdout.String()
	}

	It("warns once, naming the hosts it could not confirm", func() {
		setup(`echo "denied: not a manager" >&2; exit 1`, `exit 1`)
		out := run()
		Expect(strings.Count(out, "cannot list the hosts with a local configuration")).To(Equal(1))
		Expect(out).To(ContainSubstring("systemd not confirmed on 5 host(s): n1 n2 n3 n4 n5"))
		Expect(out).NotTo(ContainSubstring("cannot read execd_params to confirm systemd is on"))
	})

	It("still reports a single host whose own configuration cannot be read", func() {
		setup(`printf 'n2\n'`, `echo "denied" >&2; exit 1`)
		out := run()
		Expect(out).To(MatchRegexp(`(?m)^WARN  n2: cannot read execd_params to confirm systemd is on`))
		Expect(strings.Count(out, "cannot read execd_params to confirm systemd is on")).To(Equal(1))
		Expect(out).NotTo(ContainSubstring("cannot list the hosts with a local configuration"))
	})
})
