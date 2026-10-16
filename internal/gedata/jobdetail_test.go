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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// Fixtures *_9.1.6.xml are `qstat -xml -j <id>` captured on OCS 9.1.6:
//
//	running_gpu        qsub -l h_rt=1:00:00,mem_free=1G,gpu=1 -o/-e (h_rt since qalter'ed to 7200)
//	held               qsub -h, renamed and h_rt cleared by qalter
//	array              qsub -t 1-3 -h, task 2 released and running
//	begin              qsub -a 203012311200
//	dependency         qsub -hold_jid 1376
//	signal_multinode   sbatch -N 2 -n 2 -t 2:00:00 --mem=1G --signal=USR1@60, h_rt since 10800
var _ = Describe("qstat -xml -j job detail", func() {
	It("reads a running GPU job: requests in order, paths, start time, granted GPUs", func() {
		d, found, err := gedata.ParseJobDetailXML(readFixture("qstat_j_running_gpu_9.1.6.xml"), "1375")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(d.Name).To(Equal("sc_run"))
		Expect(d.Owner).To(Equal("gridware"))
		Expect(d.UID).To(Equal("1000"))
		Expect(d.HardResources).To(Equal([]gedata.Resource{
			{Name: "gpu", Value: "1"}, {Name: "h_rt", Value: "7200"}, {Name: "mem_free", Value: "1G"},
		}))
		Expect(d.Stdout).To(Equal("/tmp/sc_run.out"))
		Expect(d.Stderr).To(Equal("/tmp/sc_run.err"))
		Expect(d.MergeStderr).To(BeFalse())
		Expect(d.Home).To(Equal("/home/gridware"))
		Expect(d.Cwd).To(BeEmpty())
		Expect(d.Script).To(Equal("sleep"))
		Expect(d.Args).To(Equal([]string{"600"}))
		Expect(d.Begin.IsZero()).To(BeTrue(), "JB_execution_time 0 is no -a")
		Expect(d.TaskStart("")).To(Equal(time.Date(2026, 10, 12, 13, 23, 37, 557923000, time.Local)))
	})

	It("keeps a scoped request out of the global list qalter -l replaces", func() {
		d, _, err := gedata.ParseJobDetailXML(readFixture("qstat_j_signal_multinode_9.1.6.xml"), "1400")
		Expect(err).NotTo(HaveOccurred())
		Expect(gedata.ResourceList(d.HardResources)).To(Equal("h_rt=10800,mem_free=1G,s_rt=10740"))
		Expect(d.Cwd).To(Equal("/tmp"))
		Expect(d.MergeStderr).To(BeTrue())
		Expect(d.Stdout).To(Equal("slurm-$JOB_ID.out"))
		Expect(d.PE).To(Equal("make"))
		Expect(d.HardQueues).To(Equal([]string{"all.q"}))
	})

	It("reads holds, dependencies and a begin time", func() {
		held, _, _ := gedata.ParseJobDetailXML(readFixture("qstat_j_held_9.1.6.xml"), "1376")
		Expect(held.UserHeld("1")).To(BeTrue())
		Expect(held.HardResources).To(BeEmpty())

		arr, _, _ := gedata.ParseJobDetailXML(readFixture("qstat_j_array_9.1.6.xml"), "1377")
		Expect(arr.UserHeld("1")).To(BeTrue())
		Expect(arr.UserHeld("2")).To(BeFalse(), "task 2 was released")
		Expect(arr.UserHeld("3")).To(BeTrue())

		dep, _, _ := gedata.ParseJobDetailXML(readFixture("qstat_j_dependency_9.1.6.xml"), "1379")
		Expect(dep.Predecessors).To(Equal([]string{"1376"}))
		Expect(dep.UserHeld("1")).To(BeFalse())

		begin, _, _ := gedata.ParseJobDetailXML(readFixture("qstat_j_begin_9.1.6.xml"), "1378")
		Expect(begin.Begin).To(Equal(time.Date(2030, 12, 31, 12, 0, 0, 0, time.Local)))
	})

	It("reports an unknown job as not found, not as an error", func() {
		_, found, err := gedata.ParseJobDetailXML(readFixture("qstat_j_unknown.xml"), "999999")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})

	It("fetches with one qstat call and fills allocations and GPUs from it", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: readFixture("qstat_j_running_gpu_9.1.6.xml")}
		}}
		d, found, err := gedata.FetchJobDetail(context.Background(), r, "1375", "gpu")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(r.Calls).To(HaveLen(1))
		Expect(r.Calls[0].Args).To(Equal([]string{"-xml", "-j", "1375"}))
		Expect(d.Allocations["1375"].Hosts).To(Equal([]string{"ocs-worker1"}))
		Expect(d.GPUs).To(HaveLen(1))
		Expect(d.GPUs[0].Devices).To(Equal([]string{"0"}))
	})

	It("asks for the array job of an array-task id and surfaces qstat failures", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qmaster unreachable")}
		}}
		_, _, err := gedata.FetchJobDetail(context.Background(), r, "1377_2", "")
		Expect(err).To(MatchError(ContainSubstring("qmaster unreachable")))
		Expect(r.Calls[0].Args).To(Equal([]string{"-xml", "-j", "1377"}))

		r = &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Err: errors.New("boom")}
		}}
		_, _, err = gedata.FetchJobDetail(context.Background(), r, "1", "")
		Expect(err).To(MatchError(ContainSubstring("boom")))
	})
})
