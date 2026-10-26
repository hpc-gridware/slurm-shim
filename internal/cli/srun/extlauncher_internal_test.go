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
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
)

// The srun lines OpenMPI's mpirun sends to start its daemons. The first three
// were captured from a shim job on the Docker cluster (OpenMPI 4.1.6, and
// 5.0.9 with its bundled PRRTE 3.0.9 with and without the Grid Engine
// allocator); the last is what PRRTE master builds
// (src/mca/plm/slurm/plm_slurm_module.c), with its node file.
const (
	ompi41Line  = `--ntasks-per-node=1 --kill-on-bad-exit --mpi=none --nodes=2 --nodelist=ocs-worker2,ocs-worker1 --ntasks=2 orted -mca ess slurm -mca ess_base_vpid 1`
	prrte30Line = `--external-launcher --ntasks-per-node=1 --kill-on-bad-exit --mpi=none --cpu-bind=none --ntasks=2 prted --prtemca ess slurm --prtemca ess_base_vpid 1`
	prrte30List = `--external-launcher --ntasks-per-node=1 --kill-on-bad-exit --mpi=none --cpu-bind=none --nodes=2 --nodelist=ocs-worker2,ocs-master --ntasks=2 prted --prtemca ess slurm`
	prrteMaster = `--external-launcher --ntasks-per-node=1 --no-kill --kill-on-bad-exit=0 --mpi=none --cpu-bind=none --jobid=4711 --distribution=arbitrary --nodelist=NODEFILE --ntasks=2 prted --prtemca ess slurm`
)

var _ = Describe("srun as an MPI launcher's daemon starter", func() {
	parseLine := func(line string, strict bool) (*options, error) {
		return parseFlags(strings.Fields(line), strict, io.Discard)
	}

	nodeFile := func(content string) string {
		path := filepath.Join(GinkgoT().TempDir(), "nodes")
		Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
		return path
	}

	DescribeTable("parses each launcher's line without a warning, also under strict_flags",
		func(line, daemon string) {
			if strings.Contains(line, "NODEFILE") {
				line = strings.Replace(line, "NODEFILE", nodeFile("ocs-worker2\nocs-worker1\n"), 1)
			}
			for _, strict := range []bool{false, true} {
				opt, err := parseLine(line, strict)
				Expect(err).NotTo(HaveOccurred(), "strict=%v", strict)
				Expect(opt.warnings).To(BeEmpty(), "strict=%v", strict)
				Expect(opt.command[0]).To(Equal(daemon))
				Expect(opt.req.TasksPerNode).To(Equal(1))
				Expect(opt.req.NTasks).To(Equal(2))
			}
		},
		Entry("OpenMPI 4.1 (ORTE)", ompi41Line, "orted"),
		Entry("OpenMPI 5.0 / PRRTE 3.0, no node list", prrte30Line, "prted"),
		Entry("OpenMPI 5.0 / PRRTE 3.0, with node list", prrte30List, "prted"),
		Entry("PRRTE master, node file and arbitrary order", prrteMaster, "prted"),
	)

	It("keeps PRRTE master's node-file order and reads the kill flags as off", func() {
		line := strings.Replace(prrteMaster, "NODEFILE", nodeFile("ocs-worker2\nocs-worker1\n"), 1)
		opt, err := parseLine(line, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.req.Nodelist).To(Equal([]string{"ocs-worker2", "ocs-worker1"}))
		Expect(opt.req.Ordered).To(BeTrue())
		Expect(opt.killFlag).To(Equal("0"))
		Expect(opt.jobID).To(Equal("4711"))
	})

	Describe("--cpu-bind and SLURM_CPU_BIND", func() {
		DescribeTable("only none leaves the ranks unpinned",
			func(flag, env string, unpinned bool) {
				opt := &options{cpuBind: flag}
				applyCPUBind(opt, env)
				Expect(opt.req.NoBind).To(Equal(unpinned))
			},
			Entry("--cpu-bind=none", "none", "", true),
			Entry("SLURM_CPU_BIND=none (OpenMPI 4.1)", "", "none", true),
			Entry("a qualifier before the type", "verbose,none", "", true),
			Entry("the flag wins over the variable", "cores", "none", false),
			Entry("another type", "cores", "", false),
			Entry("nothing given", "", "", false),
		)

	})

	Describe("an --external-launcher step", func() {
		It("is never pinned or handed to mpirun, and goes past the master when no node list is given", func() {
			opt := &options{externalLauncher: true, mpi: config.MPIPMIx}
			applyExternalLauncher(opt)
			Expect(opt.req.NoBind).To(BeTrue())
			Expect(opt.req.AvoidMaster).To(BeTrue())
			Expect(opt.mpi).To(Equal(config.MPINone))

			listed := &options{externalLauncher: true}
			listed.req.Nodelist = []string{"node002"}
			applyExternalLauncher(listed)
			Expect(listed.req.AvoidMaster).To(BeFalse(), "a node list is followed as given")

			plain := &options{}
			applyExternalLauncher(plain)
			Expect(plain.req.NoBind).To(BeFalse())
			Expect(plain.req.AvoidMaster).To(BeFalse(), "an ordinary step keeps the master")
		})
	})

	Describe("the daemon step of an srun --mpi=pmix step", func() {
		lay := &layout.Layout{Nodes: []layout.Node{{Host: "m"}, {Host: "a"}, {Host: "b"}, {Host: "c"}}}

		It("runs on the pmix step's nodes other than the master when the launcher names none", func() {
			opt := &options{externalLauncher: true}
			applyExternalLauncher(opt)
			applyMPINodes(opt, lay, "a,c")
			Expect(opt.req.Nodelist).To(Equal([]string{"a", "c"}))
			Expect(opt.req.AvoidMaster).To(BeFalse())

			p, err := plan.Place(lay, plan.StepRequest{NTasks: 2, TasksPerNode: 1, Nodelist: opt.req.Nodelist})
			Expect(err).NotTo(HaveOccurred())
			Expect(joinHosts(p.Nodes)).To(Equal([]string{"a", "c"}))
		})

		It("drops the master and hosts outside the job (a stale list after a swap)", func() {
			opt := &options{}
			applyMPINodes(opt, lay, "m,b,gone")
			Expect(opt.req.Nodelist).To(Equal([]string{"b"}))
		})

		It("follows the launcher's own node list or count, and does nothing without the variable", func() {
			listed := &options{}
			listed.req.Nodelist = []string{"b"}
			applyMPINodes(listed, lay, "a,c")
			Expect(listed.req.Nodelist).To(Equal([]string{"b"}))

			counted := &options{}
			counted.req.Nodes = 2
			applyMPINodes(counted, lay, "a,c")
			Expect(counted.req.Nodelist).To(BeEmpty())

			none := &options{}
			applyMPINodes(none, lay, "")
			Expect(none.req.Nodelist).To(BeEmpty())

			onlyMaster := &options{}
			applyMPINodes(onlyMaster, lay, "m")
			Expect(onlyMaster.req.Nodelist).To(BeEmpty())
		})
	})

	Describe("--distribution", func() {
		It("refuses arbitrary without a node list, with --nodes, or with a node listed twice", func() {
			_, err := parseLine("--distribution=arbitrary -n 2 prted", false)
			Expect(err).To(MatchError(ContainSubstring("needs --nodelist")))
			_, err = parseLine("--distribution=arbitrary --nodes=2 --nodelist=a,b prted", false)
			Expect(err).To(MatchError(ContainSubstring("do not combine it with --nodes")))
			_, err = parseLine("-m arbitrary --nodelist=a,a prted", false)
			Expect(err).To(MatchError(ContainSubstring("listed twice")))
			_, err = parseLine("-m arbitrary --nodelist=a,b --ntasks-per-node=2 prted", false)
			Expect(err).To(MatchError(ContainSubstring("--ntasks-per-node=2 conflicts")))
			_, err = parseLine("-m arbitrary --nodelist=a,b -n 3 prted", false)
			Expect(err).To(MatchError(ContainSubstring("--ntasks=3, 2 entries")))
		})

		It("runs task i on the i-th entry without --ntasks-per-node", func() {
			opt, err := parseLine("-m arbitrary --nodelist=b,a -n 2 hostname", false)
			Expect(err).NotTo(HaveOccurred())
			Expect(opt.req.TasksPerNode).To(Equal(1))
			lay := &layout.Layout{
				Nodes: []layout.Node{{Host: "a", Slots: 4}, {Host: "b", Slots: 4}},
				Tasks: layout.Tasks{PerNode: []int{4, 4}},
			}
			p, err := plan.Place(lay, opt.req)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Ranks).To(HaveLen(2))
			Expect(p.Nodes[p.Ranks[0].StepNodeIndex].Host).To(Equal("b"))
			Expect(p.Nodes[p.Ranks[1].StepNodeIndex].Host).To(Equal("a"))
		})

		It("warns about any other distribution, and strict_flags refuses it", func() {
			opt, err := parseLine("--distribution=cyclic -n 2 hostname", false)
			Expect(err).NotTo(HaveOccurred())
			Expect(opt.warnings).To(ContainElement("option --distribution ignored (slurm-shim)"))
			Expect(opt.req.Ordered).To(BeFalse())
			_, err = parseLine("-m block hostname", true)
			Expect(err).To(MatchError(ContainSubstring("--distribution is not supported")))
		})
	})

	Describe("a --nodelist file", func() {
		It("reads names one per line or comma-separated, in order, and expands ranges", func() {
			hosts, err := expandNodelist(nodeFile("node003\r\nnode001, node00[4-5]\n\n"))
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts).To(Equal([]string{"node003", "node001", "node004", "node005"}))
		})

		It("names the file when it is missing, empty, too large or holds a bad name", func() {
			_, err := expandNodelist("/nonexistent/nodes")
			Expect(err).To(MatchError(ContainSubstring("/nonexistent/nodes")))
			_, err = expandNodelist(nodeFile("\n \n"))
			Expect(err).To(MatchError(ContainSubstring("no host names")))
			_, err = expandNodelist(nodeFile(strings.Repeat("a", maxNodelistFile+1)))
			Expect(err).To(MatchError(ContainSubstring("larger than")))
			_, err = expandNodelist(GinkgoT().TempDir() + "/")
			Expect(err).To(MatchError(ContainSubstring("not a regular file")))
			path := nodeFile("good\nbad;name\n")
			_, err = expandNodelist(path)
			Expect(err).To(MatchError(ContainSubstring(path)))
		})
	})

	It("refuses a FIFO as a node file instead of waiting for a writer", func() {
		fifo := filepath.Join(GinkgoT().TempDir(), "fifo")
		Expect(syscall.Mkfifo(fifo, 0o600)).To(Succeed())
		_, err := expandNodelist(fifo)
		Expect(err).To(MatchError(ContainSubstring("not a regular file")))
	})

	It("accepts --jobid only for this job", func() {
		Expect(checkJobID(&options{}, 4711)).To(Succeed())
		Expect(checkJobID(&options{jobID: "4711"}, 4711)).To(Succeed())
		Expect(checkJobID(&options{jobID: "4712"}, 4711)).To(MatchError(ContainSubstring("only inside the job srun runs in")))
		Expect(checkJobID(&options{jobID: "x"}, 4711)).To(HaveOccurred())
		Expect(checkJobID(&options{jobID: "4711"}, -1)).To(HaveOccurred(), "outside any job")
	})

	It("keeps a warning for --no-kill on an ordinary step only", func() {
		opt, err := parseLine("--no-kill -n 1 hostname", false)
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.warnings).To(ContainElement(ContainSubstring("--no-kill")))
		opt, err = parseLine("--external-launcher --no-kill -n 1 prted", false)
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.warnings).To(BeEmpty())
	})
})
