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

package gedata_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// qstat_j_squeue_9.1.6.xml is an unedited `qstat -xml -j <list>` capture from a
// live OCS 9.1.6 cluster (qmaster + two exec hosts, schedd_job_info false):
//
//	1369 sqB_held     qsub -h                       user hold
//	1371 sqD_begin    qsub -a <now+2h>              start time in the future
//	1372 sqE_arr      qsub -t 1-5 -h                user hold on every task
//	1393 sqH_ophold   -h, qhold -h o, qrls -h u     operator hold only;
//	                  -wd, -l h_rt=90:00:00,mem_free=2G
//	1394 sqA_run      -A projX -l h_rt=3600 -wd     Eqw (wd missing on the host)
//	1395 sqC_dep      -hold_jid 1394                unresolved predecessor
//	1396 sqF_arrrun   -t 1-3                        three running tasks
//	1397 sqG_pe       -pe smp 2 -l h_rt=00:30:00    running
//	1398 sqI_arrsplit -t 1-4 -h, qrls -t 2-3        tasks 1,4 held, 2,3 running
var _ = Describe("job details from qstat -xml -j [REQ-SQU-002]", func() {
	var set gedata.JobInfoSet
	BeforeEach(func() {
		var err error
		set, err = gedata.ParseJobInfoXML(readFixture("qstat_j_squeue_9.1.6.xml"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("reads every job in the document", func() {
		Expect(set.Jobs).To(HaveLen(9))
	})

	It("says the scheduler records no job information", func() {
		Expect(set.SchedulerInfo).To(BeFalse())
	})

	It("reads the account, the working directory and the PE", func() {
		Expect(set.Jobs["1394"].Account).To(Equal("projX"))
		Expect(set.Jobs["1369"].Account).To(Equal("sge"), "Grid Engine's default account")
		Expect(set.Jobs["1394"].WorkDir).To(Equal("/tmp/sqdev-out"))
		Expect(set.Jobs["1396"].WorkDir).To(Equal("/home/gridware"), "no -cwd/-wd runs in the home directory")
		Expect(set.Jobs["1397"].PE).To(Equal("smp"))
		Expect(set.Jobs["1396"].PE).To(BeEmpty())
	})

	It("reads hard resource requests with their parsed amounts", func() {
		Expect(set.Jobs["1397"].Requests["h_rt"]).To(Equal(gedata.ResourceRequest{Value: "1800", Amount: 1800}))
		Expect(set.Jobs["1393"].Requests["h_rt"].Amount).To(Equal(324000.0))
		Expect(set.Jobs["1393"].Requests["mem_free"]).To(Equal(gedata.ResourceRequest{Value: "2G", Amount: 2 << 30}))
		Expect(set.Jobs["1396"].Requests).To(BeEmpty())
	})

	It("reads an unresolved predecessor", func() {
		Expect(set.Jobs["1395"].Predecessors).To(Equal([]string{"1394"}))
		Expect(set.Jobs["1394"].Predecessors).To(BeEmpty(), "a successor list is not a predecessor list")
	})

	It("reads the -a start time and leaves it zero when there is none", func() {
		Expect(set.Jobs["1371"].Begin).To(Equal(time.Date(2026, 10, 12, 15, 23, 0, 0, time.Local)))
		Expect(set.Jobs["1369"].Begin.IsZero()).To(BeTrue())
		Expect(set.Jobs["1369"].Submit).NotTo(BeZero())
	})

	DescribeTable("tells which hold keeps a task pending",
		func(job, task, want string) {
			Expect(set.Jobs[job].Hold(task)).To(Equal(want))
		},
		Entry("a user hold", "1369", "", gedata.HoldUser),
		Entry("a user hold on a pending range", "1372", "1-5:1", gedata.HoldUser),
		Entry("an operator hold", "1393", "", gedata.HoldAdmin),
		Entry("no hold for a dependency alone", "1395", "", gedata.HoldNone),
		Entry("no hold for a -a start time", "1371", "", gedata.HoldNone),
		Entry("the held tasks of a split array", "1398", "1,4", gedata.HoldUser),
		Entry("the released task of a split array", "1398", "2", gedata.HoldNone),
	)

	It("reads the granted hosts from the same document", func() {
		Expect(set.Allocations["1397"].Hosts).To(Equal([]string{"ocs-worker1"}))
		Expect(set.Allocations).To(HaveKey("1396_3"))
		Expect(set.Allocations).NotTo(HaveKey("1369"), "a pending job has no hosts")
	})

	It("reads an unknown_jobs answer as no jobs", func() {
		set, err := gedata.ParseJobInfoXML(readFixture("qstat_j_unknown.xml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(set.Jobs).To(BeEmpty())
	})

	// The scheduler's per-job messages appear only with schedd_job_info true,
	// which the shared test cluster does not enable; this document follows the
	// layout ocs_qstat_xml.cc writes (SME_message_list, one entry per message
	// with the jobs it applies to).
	It("attaches the scheduler's messages to their jobs", func() {
		doc := `<?xml version='1.0'?>
<detailed_job_info>
  <djob_info>
    <element><JB_job_number>7</JB_job_number></element>
    <element><JB_job_number>8</JB_job_number></element>
  </djob_info>
  <messages>
    <element>
      <SME_message_list>
        <element>
          <MES_job_number_list><ulong_sublist><ULNG_value>7</ULNG_value></ulong_sublist></MES_job_number_list>
          <MES_message_number>23</MES_message_number>
          <MES_message>cannot run in PE "smp" because it only offers 0 slots</MES_message>
        </element>
      </SME_message_list>
    </element>
  </messages>
</detailed_job_info>`
		set, err := gedata.ParseJobInfoXML([]byte(doc))
		Expect(err).NotTo(HaveOccurred())
		Expect(set.SchedulerInfo).To(BeTrue())
		Expect(set.Jobs["7"].SchedulerMessages).To(ConsistOf(ContainSubstring("only offers 0 slots")))
		Expect(set.Jobs["8"].SchedulerMessages).To(BeEmpty())
	})

	It("asks qstat once, for the listed jobs, by job id", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: readFixture("qstat_j_squeue_9.1.6.xml")}
		}}
		_, err := gedata.JobInfos(context.Background(), r, []string{"1396_1", "1396_2", "1397"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Calls).To(HaveLen(1))
		Expect(r.Calls[0].Args).To(Equal([]string{"-xml", "-j", "1396,1397"}))
	})

	It("asks for every job when given none", func() {
		r := &fake.Runner{}
		_, _ = gedata.JobInfos(context.Background(), r, nil)
		Expect(r.Calls[0].Args).To(Equal([]string{"-xml", "-j", "*"}))
	})

	It("reports a failed qstat as an error", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("cannot reach qmaster")}
		}}
		_, err := gedata.JobInfos(context.Background(), r, nil)
		Expect(err).To(MatchError(ContainSubstring("cannot reach qmaster")))
	})
})

var _ = Describe("GE task lists [SI-14]", func() {
	DescribeTable("expand to their tasks",
		func(list string, want []string) {
			Expect(gedata.ExpandTasks(list)).To(Equal(want))
		},
		Entry("one task", "3", []string{"3"}),
		Entry("a range", "1-5:2", []string{"1", "3", "5"}),
		Entry("a comma list", "1,4", []string{"1", "4"}),
		Entry("ranges and tasks", "1-2:1,7", []string{"1", "2", "7"}),
	)

	DescribeTable("render as SLURM's 0-based indices",
		func(list, want string) {
			Expect(gedata.SlurmTaskList(list)).To(Equal(want))
		},
		Entry("one task", "3", "2"),
		Entry("a range", "1-10:1", "[0-9]"),
		Entry("a comma list", "1,4", "[0,3]"),
		Entry("ranges and tasks", "1-3:1,7", "[0-2,6]"),
	)

	It("finds a task in a comma list", func() {
		Expect(gedata.TaskListContains("1,4", "4")).To(BeTrue())
		Expect(gedata.TaskListContains("1,4", "2")).To(BeFalse())
		Expect(gedata.TaskListContains("1-3:1,7", "2")).To(BeTrue())
	})
})
