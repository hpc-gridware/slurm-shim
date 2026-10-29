/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

package srun_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"github.com/hpc-gridware/slurm-shim/internal/cpubind"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// fakeMpirun answers --version like Open MPI 5, reports the command line and
// the variables OpenMPI's SLURM allocator reads, then starts the rank part of
// its command line (from the shim's rank-env on) once, as rank 3, local rank 1,
// on node002 -- what a PRRTE daemon would do for that rank, in the daemon
// step's environment: a step of its own, on the daemon nodes only.
const fakeMpirun = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "mpirun (Open MPI) 5.0.9"; exit 0; fi
echo "ARGV $*"
echo "ENV $SLURM_NODELIST $SLURM_TASKS_PER_NODE $SLURM_STEP_ID $PRTE_MCA_ras $SLURM_SHIM_MPI_NODELIST"
echo "BIND ompi=$OMPI_MCA_hwloc_base_binding_policy prte=$PRTE_MCA_hwloc_default_binding_policy"
while [ $# -gt 1 ] && [ "$2" != "rank-env" ]; do shift; done
SLURMD_NODENAME=node002 PMIX_RANK=3 OMPI_COMM_WORLD_LOCAL_RANK=1 \
  SLURM_STEP_ID=1 SLURM_NTASKS=1 SLURM_STEP_NODELIST=node002 SLURM_NODEID=0 SLURM_GTIDS=0 exec "$@"
`

const fakeIntelMpirun = `#!/bin/sh
echo "Intel(R) MPI Library for Linux* OS, Version 2021.10 Build 20230619"
`

var _ = Describe("rank-env", func() {
	rankEnv := func(args ...string) *gexec.Session {
		cmd := exec.Command(shimBin, append([]string{"rank-env"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "PMIX_RANK=2", "SLURM_NODEID=7"}
		sess, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		return sess
	}

	It("sets the step's variables and drops a node index it cannot place", func() {
		sess := rankEnv("SLURM_STEP_NODELIST=nodeX", "SLURM_NTASKS=6", "--", "sh", "-c",
			`echo "procid=$SLURM_PROCID ntasks=$SLURM_NTASKS nodeid=$SLURM_NODEID"`)
		Eventually(sess, "10s").Should(gexec.Exit(0))
		Expect(lines(sess)).To(Equal([]string{"procid=2 ntasks=6 nodeid="}))
	})

	It("exits 1 without a command or with a stray word, 127 for a missing one, 126 for one it cannot run", func() {
		Eventually(rankEnv("--"), "10s").Should(gexec.Exit(1))
		Eventually(rankEnv("oops", "--", "true"), "10s").Should(gexec.Exit(1))
		Eventually(rankEnv("--", "no-such-command-here"), "10s").Should(gexec.Exit(127))
		Eventually(rankEnv("--", "/no/such/file"), "10s").Should(gexec.Exit(127))
		plain := filepath.Join(GinkgoT().TempDir(), "plain")
		Expect(os.WriteFile(plain, []byte("data"), 0o644)).To(Succeed())
		Eventually(rankEnv("--", plain), "10s").Should(gexec.Exit(126))
	})
})

var _ = Describe("srun --mpi=pmix through a launcher", func() {
	var tmp, bin string

	BeforeEach(func() {
		tmp = writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true},
			{Index: 1, Host: "node002", Slots: 4},
			{Index: 2, Host: "node003", Slots: 4},
		}, []int{4, 4, 4})
		bin = filepath.Join(tmp, "bin")
		Expect(os.MkdirAll(bin, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(bin, "mpirun"), []byte(fakeMpirun), 0o755)).To(Succeed())
	})

	withPath := func(extra ...string) []string {
		return append([]string{"PATH=" + bin + ":/usr/bin:/bin:/usr/sbin:/sbin"}, extra...)
	}
	const rankReport = `echo "RANK procid=$SLURM_PROCID localid=$SLURM_LOCALID nodeid=$SLURM_NODEID"; exit 4`
	const stepReport = `echo "STEP id=$SLURM_STEP_ID ntasks=$SLURM_NTASKS nodes=$SLURM_STEP_NODELIST gtids=$SLURM_GTIDS daemons=$SLURM_SHIM_MPI_NODELIST"`

	It("execs mpirun for the step and names each rank in SLURM's variables", func() {
		sess := runSrunEnv(tmp, withPath(), "--mpi=pmix", "-N", "2", "--ntasks-per-node=2", "sh", "-c", rankReport)
		Eventually(sess, "30s").Should(gexec.Exit(4), "the rank's exit code is the step's")
		out := lines(sess)
		Expect(out).NotTo(BeEmpty())
		Expect(out[0]).To(MatchRegexp(`^ARGV -n 4 /\S+ rank-env SLURM_NTASKS=4 .*SLURM_STEP_NODELIST=node\[001-002\] .* -- sh -c `))
		Expect(out).To(ContainElements(
			"ENV node[001-002] 2(x2) 0 slurm node[001-002]",
			"RANK procid=3 localid=1 nodeid=1",
		))
	})

	It("gives a rank forked by a daemon this step's variables, not the daemon step's", func() {
		sess := runSrunEnv(tmp, withPath(), "--mpi=pmix", "-N", "2", "--ntasks-per-node=2", "sh", "-c", stepReport)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(lines(sess)).To(ContainElement("STEP id=0 ntasks=4 nodes=node[001-002] gtids= daemons="))
	})

	It("runs a daemon step on the pmix step's nodes when the launcher names none", func() {
		// OpenMPI 4.1's line for a step that leaves out the master: no node
		// list, no --external-launcher.
		sess := runSrunEnv(tmp, []string{"SLURM_SHIM_MPI_NODELIST=node003"}, "--ntasks-per-node=1", "--mpi=none",
			"--ntasks=1", "sh", "-c", `echo "daemon on $SLURMD_NODENAME"`)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(lines(sess)).To(Equal([]string{"daemon on node003"}))
	})

	It("runs a daemon step whatever SLURM_MPI_TYPE the launcher's environment carries", func() {
		sess := runSrunEnv(tmp, []string{"SLURM_MPI_TYPE=pmi2"}, "--external-launcher", "--ntasks-per-node=1",
			"--ntasks=1", "sh", "-c", `echo "daemon on $SLURMD_NODENAME"`)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(lines(sess)).To(Equal([]string{"daemon on node002"}))
	})

	It("keeps --pty a plain step when only mpi.default asks for pmix", func() {
		Expect(os.WriteFile(filepath.Join(tmp, "config.yaml"),
			[]byte("launcher: local\nmpi:\n  default: pmix\n"), 0o600)).To(Succeed())
		sess := runSrunEnv(tmp, withPath(), "--pty", "-n", "1", "sh", "-c", `echo "own procid=$SLURM_PROCID"`)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(lines(sess)).To(Equal([]string{"own procid=0"}))
		Expect(string(sess.Err.Contents())).NotTo(ContainSubstring("mpirun"))
	})

	It("delegates a bare srun when the site's mpi.default is pmix, and SLURM_MPI_TYPE=none overrides it", func() {
		Expect(os.WriteFile(filepath.Join(tmp, "config.yaml"),
			[]byte("launcher: local\nmpi:\n  default: pmix\n"), 0o600)).To(Succeed())
		sess := runSrunEnv(tmp, withPath(), "-n", "2", "sh", "-c", rankReport)
		Eventually(sess, "30s").Should(gexec.Exit(4))
		Expect(lines(sess)).To(ContainElement("RANK procid=3 localid=1 nodeid=1"))

		sess = runSrunEnv(tmp, withPath("SLURM_MPI_TYPE=none"), "-n", "1", "sh", "-c", `echo "own procid=$SLURM_PROCID"`)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(lines(sess)).To(Equal([]string{"own procid=0"}))
	})

	It("sends a plain -o file mpirun's output", func() {
		log := filepath.Join(tmp, "mpi.log")
		sess := runSrunEnv(tmp, withPath(), "--mpi=pmix", "-n", "1", "-o", log, "sh", "-c", rankReport)
		Eventually(sess, "30s").Should(gexec.Exit(4))
		Expect(os.ReadFile(log)).To(ContainSubstring("RANK procid=3"))
	})

	It("sends -e to its own file, and one file named by -o and -e gets both streams whole", func() {
		out, errf := filepath.Join(tmp, "o.log"), filepath.Join(tmp, "e.log")
		both := `echo out-line; echo err-line >&2`
		sess := runSrunEnv(tmp, withPath(), "--mpi=pmix", "-n", "1", "-o", out, "-e", errf, "sh", "-c", both)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(os.ReadFile(out)).To(ContainSubstring("out-line"))
		Expect(os.ReadFile(errf)).To(Equal([]byte("err-line\n")))

		one := filepath.Join(tmp, "both.log")
		sess = runSrunEnv(tmp, withPath(), "--mpi=pmix", "-n", "1", "-o", one, "-e", one, "sh", "-c", both)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		data, err := os.ReadFile(one)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("out-line\n"))
		Expect(string(data)).To(ContainSubstring("err-line\n"))
	})

	It("runs mpirun in the --chdir directory, and names a -o file it cannot open", func() {
		dir := filepath.Join(tmp, "work")
		Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
		sess := runSrunEnv(tmp, withPath(), "--mpi=pmix", "-n", "1", "--chdir", dir, "-o", "rel.log", "sh", "-c", "pwd")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(os.ReadFile(filepath.Join(dir, "rel.log"))).To(ContainSubstring(dir))

		sess = runSrunEnv(tmp, withPath(), "--mpi=pmix", "-n", "1", "-o", filepath.Join(tmp, "missing", "x.log"), "true")
		Eventually(sess, "30s").Should(gexec.Exit(1))
		Expect(string(sess.Err.Contents())).To(ContainSubstring("missing/x.log"))
	})

	It("names a missing launcher and how to point at one", func() {
		sess := runSrunEnv(tmp, nil, "--mpi=pmix", "-n", "1", "true")
		Eventually(sess, "30s").Should(gexec.Exit(1))
		Expect(string(sess.Err.Contents())).To(ContainSubstring("mpirun not found"))

		Expect(os.WriteFile(filepath.Join(tmp, "config.yaml"),
			[]byte("launcher: local\nmpi:\n  launcher: "+filepath.Join(bin, "mpirun")+"\n"), 0o600)).To(Succeed())
		sess = runSrunEnv(tmp, nil, "--mpi=pmix", "-n", "1", "sh", "-c", rankReport)
		Eventually(sess, "30s").Should(gexec.Exit(4), "mpi.launcher is used without PATH")
	})

	It("reports the mpirun command under a dry run and runs nothing", func() {
		sess := runSrunEnv(tmp, withPath("SLURM_SHIM_DRY_RUN=1"), "--mpi=pmix", "-n", "6", "./app")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		report := string(sess.Err.Contents())
		Expect(report).To(ContainSubstring("mpirun (Open MPI) 5.0.9"))
		Expect(report).To(ContainSubstring("would exec:"))
		Expect(report).To(ContainSubstring("-- ./app"))
		Expect(report).To(ContainSubstring("PRTE_MCA_ras=slurm"))
		Expect(string(sess.Out.Contents())).NotTo(ContainSubstring("ARGV"))
	})

	Describe("OpenMPI's binding (CS-2945)", func() {
		// bind runs a pmix step on a one-node job holding slots and returns the
		// BIND line the fake mpirun saw.
		bind := func(slots int, wrapper []string, extraEnv ...string) string {
			job := writeAlloc([]layout.Node{{Index: 0, Host: "node001", Slots: slots, IsMaster: true}}, []int{slots})
			sess := runSrunWrapped(job, wrapper, withPath(extraEnv...), "--mpi=pmix", "-n", "1", "true")
			Eventually(sess, "30s").Should(gexec.Exit(0))
			for _, l := range lines(sess) {
				if strings.HasPrefix(l, "BIND ") {
					return l
				}
			}
			Fail("the fake mpirun printed no BIND line")
			return ""
		}
		const none = "BIND ompi=none prte=none"
		const unset = "BIND ompi= prte="

		It("leaves OpenMPI's per-core binding on a node the job owns, and stops it on a shared one", func() {
			if runtime.GOOS != "linux" {
				Expect(bind(1, nil)).To(Equal(unset), "no CPU facts off Linux: mpirun binds as before")
				return
			}
			node := thisNode()
			needs(!node.Restricted() && len(node.Allowed) >= 2, "needs an unrestricted mask with 2 CPUs")
			Expect(bind(len(node.Allowed), nil)).To(Equal(unset))
			Expect(bind(1, nil)).To(Equal(none))
		})

		It("stops OpenMPI binding outside Grid Engine's grant, unless the user chose a policy", func() {
			needs(runtime.GOOS == "linux", "affinity is applied on Linux only")
			node := thisNode()
			needs(len(node.Allowed) >= 2, "needs 2 CPUs")
			path, err := exec.LookPath("taskset")
			needs(err == nil, "taskset (util-linux) gives srun a Grid Engine-like mask")
			grant := []string{path, "-c", cpubind.FormatList(node.Allowed[len(node.Allowed)-1:])}
			Expect(bind(64, grant)).To(Equal(none), "a restricted mask, even with every slot")
			Expect(bind(64, grant, "PRTE_MCA_hwloc_default_binding_policy=core")).To(Equal("BIND ompi= prte=core"))
		})
	})

	It("refuses another MPI's mpirun, quoting it", func() {
		Expect(os.WriteFile(filepath.Join(bin, "mpirun"), []byte(fakeIntelMpirun), 0o755)).To(Succeed())
		sess := runSrunEnv(tmp, withPath(), "--mpi=pmix", "-n", "2", "./app")
		Eventually(sess, "30s").Should(gexec.Exit(1))
		Expect(string(sess.Err.Contents())).To(ContainSubstring(`says "Intel(R) MPI Library`))
	})

	It("never hands an external launcher's daemon step back to mpirun", func() {
		Expect(os.WriteFile(filepath.Join(tmp, "config.yaml"),
			[]byte("launcher: local\nmpi:\n  default: pmix\n"), 0o600)).To(Succeed())
		sess := runSrunEnv(tmp, withPath(), "--external-launcher", "--ntasks-per-node=1", "--ntasks=2",
			"sh", "-c", `echo "daemon on $SLURMD_NODENAME"`)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(lines(sess)).To(ConsistOf("daemon on node002", "daemon on node003"))
	})
})
