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

package srun

import (
	"errors"
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

// --version output captured from the Docker cluster (OpenMPI 5.0.9 built from
// source with its bundled PRRTE, the openSUSE openmpi4 package), plus the
// first lines of Intel MPI's and MPICH's for the refusal.
const (
	ompi5Version   = "mpirun (Open MPI) 5.0.9\n\nReport bugs to https://www.open-mpi.org/community/help/\n"
	ompi41Version  = "mpirun (Open MPI) 4.1.6.0.439b23db6288\n\nReport bugs to http://www.open-mpi.org/community/help/\n"
	prterunVersion = "prterun (PRRTE) 3.0.12a1\n\nReport bugs to: https://github.com/openpmix/prrte\n"
	ompi40Version  = "mpirun (Open MPI) 4.0.3\n"
	intelVersion   = "Intel(R) MPI Library for Linux* OS, Version 2021.10 Build 20230619 (id: c2e19c2f3e)\nCopyright 2003-2023, Intel Corporation.\n"
	mpichVersion   = "HYDRA build details:\n    Version:                                 4.1.2\n"
)

var _ = Describe("srun --mpi=pmix", func() {
	Describe("the MPI type", func() {
		DescribeTable("normalizes SLURM's names",
			func(in, want string) {
				got, err := normalizeMPI(in, "--mpi")
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(want))
			},
			Entry("not given", "", ""),
			Entry("none", "none", config.MPINone),
			Entry("pmix", "pmix", config.MPIPMIx),
			Entry("a versioned pmix", "pmix_v4", config.MPIPMIx),
			Entry("case folded", "PMIx", config.MPIPMIx),
		)

		It("refuses any other type, naming the supported ones", func() {
			_, err := normalizeMPI("pmi2", "--mpi")
			Expect(err).To(MatchError(ContainSubstring("--mpi=pmi2 is not supported")))
			Expect(err).To(MatchError(ContainSubstring("--mpi=pmix")))
			_, err = parseFlags([]string{"--mpi=cray_shasta", "./app"}, false, io.Discard)
			Expect(err).To(HaveOccurred())
		})

		It("takes --mpi first, then SLURM_MPI_TYPE", func() {
			Expect(resolveMPI(config.MPINone, "pmix")).To(Equal(config.MPINone))
			Expect(resolveMPI("", "pmix_v5")).To(Equal(config.MPIPMIx))
			Expect(resolveMPI("", "none")).To(Equal(config.MPINone))
			Expect(resolveMPI("", "")).To(Equal(""))
			_, err := resolveMPI("", "pmi2")
			Expect(err).To(MatchError(ContainSubstring("SLURM_MPI_TYPE=pmi2")))
		})

		It("applies mpi.default to a step that named no type, but not to --pty", func() {
			pmix := &config.Config{MPI: config.MPI{Default: config.MPIPMIx}}
			none := &config.Config{MPI: config.MPI{Default: config.MPINone}}

			opt := &options{}
			applyMPIDefault(opt, pmix)
			Expect(opt.mpi).To(Equal(config.MPIPMIx))

			opt = &options{}
			applyMPIDefault(opt, none)
			Expect(opt.mpi).To(BeEmpty())

			opt = &options{mpi: config.MPINone}
			applyMPIDefault(opt, pmix)
			Expect(opt.mpi).To(Equal(config.MPINone))

			opt = &options{pty: true}
			applyMPIDefault(opt, pmix)
			Expect(opt.mpi).To(BeEmpty())
		})
	})

	Describe("the launcher", func() {
		It("identifies Open MPI 5, 4.1 and prterun", func() {
			l, err := parseLauncher("/x/mpirun", ompi5Version)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.prrte).To(BeTrue())
			Expect(l.banner).To(Equal("mpirun (Open MPI) 5.0.9"))

			l, err = parseLauncher("/x/mpirun", ompi41Version)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.prrte).To(BeFalse())
			Expect(l.banner).To(Equal("mpirun (Open MPI) 4.1.6.0.439b23db6288"))

			l, err = parseLauncher("/x/prterun", prterunVersion)
			Expect(err).NotTo(HaveOccurred())
			Expect(l.prrte).To(BeTrue())
			Expect(l.banner).To(Equal("prterun (PRRTE) 3.0.12a1"))

			l, err = parseLauncher("/x/mpirun", "mpirun (Open MPI) 5.0.0rc1\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(l.prrte).To(BeTrue())
		})

		DescribeTable("refuses anything else, quoting what it found",
			func(out, want string) {
				_, err := parseLauncher("/x/mpirun", out)
				Expect(err).To(MatchError(ContainSubstring(want)))
			},
			Entry("Open MPI older than 4.1", ompi40Version, "needs Open MPI 4.1 or newer"),
			Entry("Intel MPI", intelVersion, `"Intel(R) MPI Library for Linux* OS`),
			Entry("MPICH", mpichVersion, `"HYDRA build details:"`),
			Entry("no output", "", "no version output"),
		)
	})

	Describe("the mpirun command line", func() {
		step := &plan.StepPlan{NTasks: 6, CPUsPerTask: 1}

		It("runs one rank per task, each through rank-env with the step's variables", func() {
			l := &mpiLauncher{path: "/ompi/bin/mpirun", prrte: true}
			vars := []string{"SLURM_NTASKS=6", "SLURM_STEP_ID=0"}
			Expect(launcherArgv(l, step, false, "/shim/bin/slurm-shim", vars, []string{"./app", "-x"})).To(Equal([]string{
				"/ompi/bin/mpirun", "-n", "6", "/shim/bin/slurm-shim", "rank-env",
				"SLURM_NTASKS=6", "SLURM_STEP_ID=0", "--", "./app", "-x"}))
		})

		It("maps -c and -l in each version's spelling", func() {
			wide := &plan.StepPlan{NTasks: 4, CPUsPerTask: 2}
			five := launcherArgv(&mpiLauncher{path: "m", prrte: true}, wide, true, "s", nil, []string{"a"})
			Expect(five).To(Equal([]string{"m", "-n", "4", "--map-by", "slot:pe=2", "--output", "tag", "s", "rank-env", "--", "a"}))
			four := launcherArgv(&mpiLauncher{path: "m"}, wide, true, "s", nil, []string{"a"})
			Expect(four).To(Equal([]string{"m", "-n", "4", "--map-by", "slot:PE=2", "--tag-output", "s", "rank-env", "--", "a"}))
		})
	})

	Describe("the step's variables", func() {
		lay := &layout.Layout{Nodes: []layout.Node{{Host: "node001"}, {Host: "node002"}, {Host: "node003"}}}
		p := &plan.StepPlan{
			Nodes:       []plan.StepNode{{Host: "node002", LayoutIndex: 1}, {Host: "node003", LayoutIndex: 2}},
			Ranks:       []plan.PlacedRank{{StepNodeIndex: 0}, {StepNodeIndex: 0}, {StepNodeIndex: 1}, {StepNodeIndex: 1}},
			NTasks:      4,
			CPUsPerTask: 1,
		}
		vars := stepVars(lay, p, 3)

		It("name the step, its nodes and tasks per node", func() {
			Expect(vars).To(ContainElements(
				"SLURM_NODELIST=node[002-003]",
				"SLURM_STEP_NODELIST=node[002-003]",
				"SLURM_TASKS_PER_NODE=2(x2)",
				"SLURM_NTASKS=4",
				"SLURM_STEP_ID=3",
			))
		})

		It("go over the job's in mpirun's environment, with OpenMPI's SLURM allocator and the daemon nodes", func() {
			env := launcherEnv([]string{"SLURM_NODELIST=node[001-003]", "SLURM_TASKS_PER_NODE=4(x3)", "HOME=/home/u"}, vars, p)
			Expect(env).To(ContainElements(
				"SLURM_NODELIST=node[002-003]",
				"SLURM_TASKS_PER_NODE=2(x2)",
				"SLURM_SHIM_MPI_NODELIST=node[002-003]",
				"PRTE_MCA_ras=slurm",
				"OMPI_MCA_ras=slurm",
				"HOME=/home/u",
			))
			Expect(env).NotTo(ContainElement("SLURM_NODELIST=node[001-003]"))
		})
	})

	DescribeTable("refuses what mpirun cannot honour",
		func(opt *options, want string) {
			refusal, _ := pmixRefusal(opt)
			Expect(refusal).To(ContainSubstring(want))
		},
		Entry("--pty", &options{pty: true}, "--pty"),
		Entry("--x-elastic=on", &options{elastic: config.ElasticOn}, "losing a rank"),
		Entry("a per-rank -o pattern", &options{output: "out.%t"}, "per-rank output patterns"),
		Entry("a per-rank -e pattern", &options{errorPat: "err.%n"}, "per-rank output patterns"),
	)

	It("runs, with a warning, what it runs differently", func() {
		opt := &options{output: "plain.log", gpuBind: "per_task:1", exportSpec: "NONE"}
		refusal, warnings := pmixRefusal(opt)
		Expect(refusal).To(BeEmpty())
		Expect(warnings).To(ContainElements(ContainSubstring("--gpu-bind"), ContainSubstring("--export")))
	})

	Describe("rank-env", func() {
		env := func(kv map[string]string) func(string) string {
			return func(k string) string { return kv[k] }
		}
		noHost := func() (string, error) { return "", errors.New("no hostname") }

		It("names the rank from PMIx, its local rank from OpenMPI and its node from the step list", func() {
			Expect(rankOverlay(env(map[string]string{
				"PMIX_RANK":                  "5",
				"OMPI_COMM_WORLD_RANK":       "9",
				"OMPI_COMM_WORLD_LOCAL_RANK": "1",
				"SLURMD_NODENAME":            "node003",
				"SLURM_STEP_NODELIST":        "node[002-003]",
			}), noHost)).To(Equal([]string{"SLURM_PROCID=5", "SLURM_LOCALID=1", "SLURM_NODEID=1"}))
		})

		It("falls back to OpenMPI's rank and the host name, matching short and full names", func() {
			Expect(rankOverlay(env(map[string]string{
				"OMPI_COMM_WORLD_RANK": "2",
				"SLURM_STEP_NODELIST":  "node[001-002]",
			}), func() (string, error) { return "node002.cluster", nil })).To(Equal([]string{"SLURM_PROCID=2", "SLURM_NODEID=1"}))
		})

		It("adds nothing outside mpirun", func() {
			Expect(rankOverlay(env(map[string]string{}), noHost)).To(BeEmpty())
		})

		It("gives no node index to a host outside the step", func() {
			Expect(rankOverlay(env(map[string]string{
				"PMIX_RANK":           "0",
				"SLURMD_NODENAME":     "node009",
				"SLURM_STEP_NODELIST": "node[001-002]",
			}), noHost)).To(Equal([]string{"SLURM_PROCID=0"}))
		})

		It("reads the last value of a variable", func() {
			get := envLookup([]string{"A=1", "B=2", "A=3", "C=x=y"})
			Expect(get("A")).To(Equal("3"))
			Expect(get("C")).To(Equal("x=y"))
			Expect(get("D")).To(BeEmpty())
		})
	})
})
