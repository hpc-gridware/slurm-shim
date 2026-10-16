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

package scontrol

import (
	"bytes"
	"io"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// geFixture reads a Grid Engine capture from gedata's testdata (OCS 9.1.6,
// see internal/gedata/jobdetail_test.go for how each job was submitted).
func geFixture(name string) []byte {
	data, err := os.ReadFile("../../gedata/testdata/" + name)
	Expect(err).NotTo(HaveOccurred())
	return data
}

// liveCluster answers the listing with the captured `qstat -xml -u gridware`
// and each `qstat -xml -j <id>` with that job's capture.
func liveCluster(name string, args []string) fake.Response {
	details := map[string]string{
		"1376": "qstat_j_held_9.1.6.xml",
		"1378": "qstat_j_begin_9.1.6.xml",
		"1379": "qstat_j_dependency_9.1.6.xml",
		"1399": "qstat_j_running_unlimited_9.1.6.xml",
		"1400": "qstat_j_signal_multinode_9.1.6.xml",
	}
	joined := name + " " + strings.Join(args, " ")
	switch {
	case name == "qalter":
		return fake.Response{Stdout: []byte("modified hard resource list of job " + args[len(args)-1])}
	case joined == "qstat -xml -u *":
		return fake.Response{Stdout: geFixture("qstat_xml_list_9.1.6.xml")}
	case strings.HasPrefix(joined, "qstat -xml -j "):
		if f, ok := details[args[2]]; ok {
			return fake.Response{Stdout: geFixture(f)}
		}
		return fake.Response{Stdout: geFixture("qstat_j_unknown.xml")}
	}
	return clusterResponse(name, args)
}

// testConfig is the live cluster's partition layout.
func testConfig() *config.Config {
	cfg := config.Default()
	cfg.DefaultPartition = "batch"
	cfg.Partitions = map[string]config.Partition{
		"batch": {Queue: "all.q", PE: "make", Slots: "per-task"},
		"smp":   {Queue: "all.q", PE: "smp", Slots: "per-task"},
	}
	return cfg
}

func showJobOutput(args ...string) (string, string, int) {
	GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir()) // no in-job layout
	var out, errOut bytes.Buffer
	code := run(&fake.Runner{Responder: liveCluster}, testConfig(), append([]string{"show", "job"}, args...), &out, &errOut)
	return out.String(), errOut.String(), code
}

var _ = Describe("scontrol show job: full record [REQ-SCT-003]", func() {
	BeforeEach(func() {
		// Ten minutes after the captured jobs started.
		pinned := time.Date(2026, 10, 12, 13, 48, 31, 484553000, time.Local)
		now = func() time.Time { return pinned }
		DeferCleanup(func() { now = time.Now })
	})

	It("renders a running multi-node sbatch job in SLURM's layout", func() {
		out, errOut, code := showJobOutput("1400")
		Expect(code).To(Equal(0))
		Expect(errOut).To(BeEmpty())
		Expect(out).To(Equal(`JobId=1400 JobName=sc_sb2
   UserId=gridware(1000) GroupId=gridware(1000)
   JobState=RUNNING Reason=None Dependency=(null)
   ExitCode=0:0
   RunTime=00:09:59 TimeLimit=03:00:00
   SubmitTime=2026-10-12T13:38:31 EligibleTime=2026-10-12T13:38:31
   StartTime=2026-10-12T13:38:31
   Partition=batch
   NodeList=ocs-master,ocs-worker1
   NumNodes=2 NumCPUs=2 NumTasks=2
   ReqTRES=cpu=2,mem=2G,node=2
   AllocTRES=cpu=2,mem=2G,node=2
   Command=/tmp/sc_job.sh
   WorkDir=/tmp
   StdErr=/tmp/slurm-1400.out
   StdOut=/tmp/slurm-1400.out

`))
	})

	It("prints one line with -o, before or after the subcommand", func() {
		for _, args := range [][]string{{"-o", "show", "job", "1400"}, {"show", "--oneliner", "job", "1400"}} {
			GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir())
			var out bytes.Buffer
			Expect(run(&fake.Runner{Responder: liveCluster}, testConfig(), args, &out, io.Discard)).To(Equal(0))
			Expect(strings.Count(out.String(), "\n")).To(Equal(1), "args %v", args)
			Expect(out.String()).To(HavePrefix("JobId=1400 JobName=sc_sb2 UserId=gridware(1000) "))
		}
	})

	It("shows an unlimited GPU job with its granted GPU in AllocTRES", func() {
		out, _, code := showJobOutput("1399")
		Expect(code).To(Equal(0))
		Expect(out).To(ContainSubstring("TimeLimit=UNLIMITED"))
		Expect(out).To(ContainSubstring("AllocTRES=cpu=1,mem=1G,node=1,gres/gpu=1"))
		Expect(out).To(ContainSubstring("StdOut=/tmp/sc_run2.out"))
		Expect(out).To(ContainSubstring("StdErr=/tmp/sc_run2.err"))
		Expect(out).To(ContainSubstring("WorkDir=/home/gridware"), "no -cwd: GE runs the job in the owner's home")
	})

	DescribeTable("maps why a pending job waits to Reason",
		func(id, reason, extra string) {
			out, _, code := showJobOutput(id)
			Expect(code).To(Equal(0))
			Expect(out).To(ContainSubstring("JobState=PENDING Reason=" + reason + " "))
			Expect(out).To(ContainSubstring("RunTime=00:00:00"))
			Expect(out).To(ContainSubstring("StartTime=Unknown"))
			Expect(out).To(ContainSubstring("AllocTRES=(null)"))
			Expect(out).To(ContainSubstring(extra))
		},
		Entry("qsub -h is a user hold", "1376", "JobHeldUser", "StdOut=/home/gridware/newname.o1376"),
		Entry("-hold_jid is a dependency", "1379", "Dependency", "Dependency=afterany:1376"),
		Entry("-a in the future is a begin time", "1378", "BeginTime", "EligibleTime=2030-12-31T12:00:00"),
	)
})

var _ = Describe("show job helpers", func() {
	DescribeTable("SLURM durations",
		func(secs int, want string) { Expect(slurmDuration(secs)).To(Equal(want)) },
		Entry("minutes", 600, "00:10:00"),
		Entry("hours", 7200, "02:00:00"),
		Entry("a day and more", 90061, "1-01:01:01"),
	)

	DescribeTable("GE TIME values",
		func(v string, want int, ok bool) {
			got, gotOK := geSeconds(v)
			Expect(gotOK).To(Equal(ok))
			Expect(got).To(Equal(want))
		},
		Entry("seconds", "7200", 7200, true),
		Entry("H:M:S", "2:00:00", 7200, true),
		Entry("M:S", "1:30", 90, true),
		Entry("INFINITY", "INFINITY", 0, false),
		Entry("garbage", "soon", 0, false),
	)

	It("scales an integer memory request by the node count", func() {
		Expect(scaleMemory("2G", 3)).To(Equal("6G"))
		Expect(scaleMemory("1.5G", 2)).To(Equal("1.5G"), "a fraction is shown as requested")
		Expect(scaleMemory("512M", 1)).To(Equal("512M"))
	})
})

var _ = Describe("scontrol show job on OCS 9.0 [gedata.HoldKinds]", func() {
	// OCS 9.0.10 leaves the hold ranges out of qstat -xml -j: without asking
	// qstat -s, a user hold would read as an operator's (JobHeldAdmin).
	ocs9010 := func(_ string, args []string) fake.Response {
		switch strings.Join(args, " ") {
		case "-xml -u *":
			return fake.Response{Stdout: geFixture("qstat_xml_held_9.0.10.xml")}
		case "-xml -j 1":
			return fake.Response{Stdout: geFixture("qstat_j_held_9.0.10.xml")}
		case "-xml -u * -s hu":
			return fake.Response{Stdout: geFixture("qstat_s_hu_9.0.10.xml")}
		case "-xml -u * -s ho", "-xml -u * -s hs":
			return fake.Response{Stdout: geFixture("qstat_s_ho_empty_9.0.10.xml")}
		}
		return fake.Response{Exit: 1, Stderr: []byte("unexpected")}
	}

	It("reports a user hold as JobHeldUser", func() {
		GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir())
		var out, errOut bytes.Buffer
		Expect(run(&fake.Runner{Responder: ocs9010}, testConfig(), []string{"show", "job", "1"}, &out, &errOut)).To(Equal(0), errOut.String())
		Expect(out.String()).To(ContainSubstring("JobState=PENDING Reason=JobHeldUser"))
		Expect(errOut.String()).To(BeEmpty())
	})

	It("does not ask qstat -s when the detail names the hold (OCS 9.1)", func() {
		GinkgoT().Setenv("TMPDIR", GinkgoT().TempDir())
		r := &fake.Runner{Responder: liveCluster}
		Expect(run(r, testConfig(), []string{"show", "job", "1376"}, io.Discard, io.Discard)).To(Equal(0))
		for _, c := range r.Calls {
			Expect(c.Args).NotTo(ContainElement("-s"))
		}
	})
})
