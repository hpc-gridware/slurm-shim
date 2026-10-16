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

package squeue

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func TestSqueue(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Squeue Suite")
}

const qstatXML = `<?xml version='1.0'?>
<job_info>
  <queue_info>
    <job_list state="running">
      <JB_job_number>4711</JB_job_number>
      <JB_name>train</JB_name>
      <JB_owner>alice</JB_owner>
      <state>r</state>
      <queue_name>gpu.q@node001</queue_name>
      <slots>8</slots>
    </job_list>
  </queue_info>
  <job_info>
    <job_list state="pending">
      <JB_job_number>4712</JB_job_number>
      <JB_name>test</JB_name>
      <JB_owner>bob</JB_owner>
      <state>qw</state>
      <slots>4</slots>
    </job_list>
    <job_list state="pending">
      <JB_job_number>4713</JB_job_number>
      <JB_name>arr</JB_name>
      <JB_owner>alice</JB_owner>
      <state>qw</state>
      <slots>1</slots>
      <tasks>2</tasks>
    </job_list>
  </job_info>
</job_info>`

func testCfg() *config.Config {
	c := config.Default()
	c.PartitionAliases = map[string]string{"gpu.q": "gpu", "all.q": "batch"}
	return c
}

func fakeQstat() *fake.Runner {
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		return fake.Response{Stdout: []byte(qstatXML)}
	}}
}

var _ = Describe("squeue [REQ-SQU-002]", func() {
	It("renders the default 8-column header and mapped states", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), nil, &out, io.Discard)).To(Equal(0))
		s := out.String()
		Expect(s).To(HavePrefix("             JOBID PARTITION"))
		Expect(s).To(ContainSubstring("NODELIST(REASON)"))
		// Running job maps to R with its partition alias.
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		Expect(lines).To(HaveLen(4)) // header + 3 jobs
	})

	It("maps GE states to SLURM compact states [REQ-SQU-001]", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-o", "%i %t"}, &out, io.Discard)).To(Equal(0))
		rows := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		Expect(rows).To(ContainElement("4711 R"))
		Expect(rows).To(ContainElement("4712 PD"))
		Expect(rows).To(ContainElement("4713_1 PD")) // GE task 2 is SLURM index 1 (SI-14)
	})

	It("maps a running job's queue to its partition alias [REQ-SQU-002]", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-j", "4711", "-o", "%i %P %R"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("4711 gpu node001"))
	})

	It("filters to a single job with -j", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-j", "4711", "-o", "%i"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("4711"))
	})

	It("filters to a single array task by its 0-based SLURM index", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-j", "4713_1", "-o", "%i"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("4713_1"))
	})

	It("shows only the header for a completed (absent) job [REQ-SQU-003]", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-j", "9999", "-o", "%i %t"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimRight(out.String(), "\n")).To(Equal("JOBID ST"))
	})

	It("suppresses the header with -h", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-o", "%i"}, &out, io.Discard)).To(Equal(0))
		Expect(out.String()).NotTo(ContainSubstring("JOBID"))
	})

	It("surfaces a qstat failure", func() {
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qstat: cannot connect")}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, testCfg(), nil, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("cannot connect"))
	})
})

// runningXML is the plain `qstat -xml` view. Job 23 has started, so it has a
// start time to measure from; the default fixture above has none, which is the
// pending case. Job 26 is suspended and 27 is completing: Grid Engine keeps a
// start time and the granted hosts for both.
//
// Every job's queue_name here is its MASTER queue instance, which is all the
// plain view carries. That is the whole reason for the second query.
const runningXML = `<?xml version='1.0'?>
<job_info>
  <queue_info>
    <job_list state="running">
      <JB_job_number>23</JB_job_number>
      <JB_name>multi</JB_name>
      <JB_owner>gridware</JB_owner>
      <state>r</state>
      <JAT_start_time>2020-01-02T03:04:05.000000</JAT_start_time>
      <queue_name>all.q@ocs-master</queue_name>
      <slots>4</slots>
    </job_list>
    <job_list state="running">
      <JB_job_number>24</JB_job_number>
      <JB_name>arr</JB_name>
      <JB_owner>gridware</JB_owner>
      <state>r</state>
      <JAT_start_time>2020-01-02T03:04:05.000000</JAT_start_time>
      <queue_name>all.q@ocs-master</queue_name>
      <slots>2</slots>
      <tasks>2</tasks>
    </job_list>
    <job_list state="running">
      <JB_job_number>26</JB_job_number>
      <JB_name>susp</JB_name>
      <JB_owner>gridware</JB_owner>
      <state>s</state>
      <JAT_start_time>2020-01-02T03:04:05.000000</JAT_start_time>
      <queue_name>all.q@ocs-master</queue_name>
      <slots>2</slots>
    </job_list>
    <job_list state="running">
      <JB_job_number>27</JB_job_number>
      <JB_name>comp</JB_name>
      <JB_owner>gridware</JB_owner>
      <state>dr</state>
      <JAT_start_time>2020-01-02T03:04:05.000000</JAT_start_time>
      <queue_name>all.q@ocs-master</queue_name>
      <slots>2</slots>
    </job_list>
  </queue_info>
  <job_info>
    <job_list state="pending">
      <JB_job_number>25</JB_job_number>
      <JB_name>pend</JB_name>
      <JB_owner>gridware</JB_owner>
      <state>qw</state>
      <slots>2</slots>
    </job_list>
  </job_info>
</job_info>`

// detailedXML is the `qstat -xml -j` view of the same jobs. The grant order is
// deliberately NOT alphabetical: ocs-worker2 was granted first, and job 23's
// master instance (ocs-master, above) is not the first host. A sorted encoding
// would therefore render visibly differently, which is how the specs below can
// tell that the node list is left in the scheduler's order (REQ-ENC-002, SI-41).
const detailedXML = `<?xml version='1.0'?>
<detailed_job_info>
  <djob_info>
    <element>
      <JB_job_number>23</JB_job_number>
      <JB_ja_tasks>
        <element>
          <JAT_task_number>1</JAT_task_number>
          <JAT_granted_destin_identifier_list>
            <element><JG_qname>all.q@ocs-worker2</JG_qname><JG_qhostname>ocs-worker2</JG_qhostname><JG_slots>2</JG_slots></element>
            <element><JG_qname>all.q@ocs-master</JG_qname><JG_qhostname>ocs-master</JG_qhostname><JG_slots>1</JG_slots></element>
            <element><JG_qname>all.q@ocs-worker1</JG_qname><JG_qhostname>ocs-worker1</JG_qhostname><JG_slots>1</JG_slots></element>
          </JAT_granted_destin_identifier_list>
        </element>
      </JB_ja_tasks>
    </element>
    <element>
      <JB_job_number>24</JB_job_number>
      <JB_ja_tasks>
        <element>
          <JAT_task_number>1</JAT_task_number>
          <JAT_granted_destin_identifier_list>
            <element><JG_qhostname>ocs-master</JG_qhostname><JG_slots>2</JG_slots></element>
          </JAT_granted_destin_identifier_list>
        </element>
        <element>
          <JAT_task_number>2</JAT_task_number>
          <JAT_granted_destin_identifier_list>
            <element><JG_qhostname>ocs-worker2</JG_qhostname><JG_slots>1</JG_slots></element>
            <element><JG_qhostname>ocs-worker1</JG_qhostname><JG_slots>1</JG_slots></element>
          </JAT_granted_destin_identifier_list>
        </element>
      </JB_ja_tasks>
    </element>
    <element>
      <JB_job_number>26</JB_job_number>
      <JB_ja_tasks>
        <element>
          <JAT_task_number>1</JAT_task_number>
          <JAT_granted_destin_identifier_list>
            <element><JG_qhostname>ocs-worker2</JG_qhostname><JG_slots>1</JG_slots></element>
            <element><JG_qhostname>ocs-worker1</JG_qhostname><JG_slots>1</JG_slots></element>
          </JAT_granted_destin_identifier_list>
        </element>
      </JB_ja_tasks>
    </element>
    <element>
      <JB_job_number>27</JB_job_number>
      <JB_ja_tasks>
        <element>
          <JAT_task_number>1</JAT_task_number>
          <JAT_granted_destin_identifier_list>
            <element><JG_qhostname>ocs-worker2</JG_qhostname><JG_slots>1</JG_slots></element>
            <element><JG_qhostname>ocs-worker1</JG_qhostname><JG_slots>1</JG_slots></element>
          </JAT_granted_destin_identifier_list>
        </element>
      </JB_ja_tasks>
    </element>
  </djob_info>
</detailed_job_info>`

// isDetailed reports whether an argv is the supplementary `qstat -xml -j` call.
func isDetailed(args []string) bool {
	for _, a := range args {
		if a == "-j" {
			return true
		}
	}
	return false
}

// twoViewQstat answers each qstat view with its own output, so a spec cannot
// pass by accident on a fake that returns the same bytes to every call.
func twoViewQstat() *fake.Runner {
	return &fake.Runner{Responder: func(_ string, args []string) fake.Response {
		if isDetailed(args) {
			return fake.Response{Stdout: []byte(detailedXML)}
		}
		return fake.Response{Stdout: []byte(runningXML)}
	}}
}

var _ = Describe("squeue node columns [REQ-SQU-002]", func() {
	It("reports every host a multi-node job occupies, not just the master", func() {
		// The defect this fixes: a three-host job used to read as one node on
		// ocs-master, because plain qstat -xml carries only the MASTER instance.
		var out bytes.Buffer
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "23", "-o", "%i %D %N"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("23 3 ocs-worker2,ocs-master,ocs-worker1"))
	})

	It("leaves the node list in the order the scheduler granted it", func() {
		// REQ-ENC-002 as amended by SI-41: the encoding is first-seen order. A
		// sorted list would render "ocs-master,ocs-worker[1-2]" here, and would
		// desynchronise a master address derived from this column from the rank-0
		// host the job itself uses.
		var out bytes.Buffer
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "23", "-o", "%N"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).NotTo(Equal("ocs-master,ocs-worker[1-2]"))
		Expect(strings.TrimSpace(out.String())).To(Equal("ocs-worker2,ocs-master,ocs-worker1"))
	})

	It("does not reorder the host list it was given", func() {
		// The slice belongs to the host map, and the default format renders a node
		// column more than once per row; a renderer that sorted in place would
		// change what the next verb sees.
		hosts := []string{"ocs-worker2", "ocs-master", "ocs-worker1"}
		v := view{cfg: testCfg(), hosts: map[string]gedata.JobAllocation{"23": {Hosts: hosts}}, now: time.Now()}
		row := gedata.JobRow{JobID: "23", State: "r"}
		Expect(v.rowValue('N', row)).To(Equal(v.rowValue('N', row)))
		Expect(hosts).To(Equal([]string{"ocs-worker2", "ocs-master", "ocs-worker1"}))
	})

	It("matches an array element to its own hosts", func() {
		// The producer writes the host map keys and the consumer reads them. If the
		// two ever disagree the lookup misses silently and the row renders as the
		// original bug -- one node, the master host. This is the spec that ties
		// them together: job 24 task 2 is on two hosts, neither of them the master
		// instance the plain view reports.
		var out bytes.Buffer
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "24_1", "-o", "%i %D %N"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("24_1 2 ocs-worker[2,1]"))
	})

	It("leaves CPUS alone", func() {
		// CPUS must keep coming from the plain view's slot count, which is 4 here,
		// not from the number of granted hosts, which is 3.
		var out bytes.Buffer
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "23", "-o", "%i %C"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("23 4"))
	})

	It("reports no host and no count for a pending job", func() {
		var out bytes.Buffer
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "25", "-o", "%i %D %R"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("25 1 (None)"))
	})

	It("keeps the allocation visible for a suspended job", func() {
		// Suspension is routine on any site using subordinate queues, which is how
		// Grid Engine implements preemption. SLURM keeps showing the allocation.
		var out bytes.Buffer
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "26", "-o", "%i %t %D %R"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("26 S 2 ocs-worker[2,1]"))
	})

	It("keeps the allocation visible for a completing job", func() {
		var out bytes.Buffer
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "27", "-o", "%i %t %D %R"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("27 CG 2 ocs-worker[2,1]"))
	})

	It("skips the supplementary query when no node column was asked for", func() {
		r := twoViewQstat()
		var out bytes.Buffer
		Expect(run(r, testCfg(), []string{"-h", "-o", "%i %t"}, &out, io.Discard)).To(Equal(0))
		for _, c := range r.Calls {
			Expect(isDetailed(c.Args)).To(BeFalse(),
				"a format without a node column must cost one qstat, not two")
		}
	})

	It("narrows the supplementary query to the job that was asked for", func() {
		// squeue is the most-polled command on a cluster. -j must not cost a
		// cluster-wide detail query.
		r := twoViewQstat()
		var out bytes.Buffer
		Expect(run(r, testCfg(), []string{"-h", "-j", "24_1", "-o", "%i %N"}, &out, io.Discard)).To(Equal(0))
		var detailed [][]string
		for _, c := range r.Calls {
			if isDetailed(c.Args) {
				detailed = append(detailed, c.Args)
			}
		}
		Expect(detailed).To(Equal([][]string{{"-xml", "-j", "24"}}))
	})

	It("asks about exactly the jobs it lists, in one query", func() {
		r := twoViewQstat()
		var out bytes.Buffer
		Expect(run(r, testCfg(), []string{"-h", "-o", "%i %N"}, &out, io.Discard)).To(Equal(0))
		var detailed [][]string
		for _, c := range r.Calls {
			if isDetailed(c.Args) {
				detailed = append(detailed, c.Args)
			}
		}
		Expect(detailed).To(Equal([][]string{{"-xml", "-j", "23,24,26,27,25"}}))
	})

	It("asks for every job when the listing is long", func() {
		var many strings.Builder
		many.WriteString("<?xml version='1.0'?><job_info><queue_info></queue_info><job_info>")
		for id := 1; id <= maxDetailIDs+1; id++ {
			many.WriteString("<job_list><JB_job_number>" + strconv.Itoa(id) + "</JB_job_number><state>qw</state></job_list>")
		}
		many.WriteString("</job_info></job_info>")
		r := &fake.Runner{Responder: func(_ string, args []string) fake.Response {
			if isDetailed(args) {
				return fake.Response{Stdout: []byte(readEmptyDetail)}
			}
			return fake.Response{Stdout: []byte(many.String())}
		}}
		Expect(run(r, testCfg(), []string{"-h"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls).To(HaveLen(2))
		Expect(r.Calls[1].Args).To(Equal([]string{"-xml", "-j", "*"}))
	})

	It("makes no detail query when nothing is listed", func() {
		r := twoViewQstat()
		Expect(run(r, testCfg(), []string{"-j", "9999"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls).To(HaveLen(1))
	})

	It("still lists jobs when the supplementary query fails, and says so", func() {
		// A node list is worth a warning, never a failed listing -- but the fallback
		// renders exactly like a correct single-node answer, so it must not pass
		// silently either.
		r := &fake.Runner{Responder: func(_ string, args []string) fake.Response {
			if isDetailed(args) {
				return fake.Response{Exit: 1, Stderr: []byte("qmaster unreachable")}
			}
			return fake.Response{Stdout: []byte(runningXML)}
		}}
		var out, errOut bytes.Buffer
		Expect(run(r, testCfg(), []string{"-h", "-j", "23", "-o", "%i %D %N"}, &out, &errOut)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("23 1 ocs-master"), "falls back to the master instance")
		Expect(errOut.String()).To(ContainSubstring("job details unavailable"))
		Expect(errOut.String()).To(ContainSubstring("no granted host list for 23"))
	})

	It("says so when a started job is simply missing from the host map", func() {
		// The job started between the two queries, so it is running in one view and
		// absent from the other. Without the warning this is indistinguishable from
		// a real single-node job.
		r := &fake.Runner{Responder: func(_ string, args []string) fake.Response {
			if isDetailed(args) {
				return fake.Response{Stdout: []byte(readEmptyDetail)}
			}
			return fake.Response{Stdout: []byte(runningXML)}
		}}
		var out, errOut bytes.Buffer
		Expect(run(r, testCfg(), []string{"-h", "-j", "23", "-o", "%i %D %N"}, &out, &errOut)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("23 1 ocs-master"))
		Expect(errOut.String()).To(ContainSubstring("no granted host list for 23"))
	})

	It("says nothing about a pending job, which has no allocation to miss", func() {
		var out, errOut bytes.Buffer
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "25", "-o", "%i %N"}, &out, &errOut)).To(Equal(0))
		Expect(errOut.String()).To(BeEmpty())
	})

	It("gates the supplementary query on exactly the verbs that consume it", func() {
		// needsDetail lists the verbs that trigger the detail query and rowValue
		// reads the detail for some set of verbs. If they drift apart, a column is
		// either always empty or costs a query nothing reads. Every detail field
		// is set below, so each detail verb renders differently with and without.
		running := gedata.JobRow{JobID: "23", State: "r", Queue: "all.q@ocs-master", Slots: 4,
			Start: time.Date(2026, 9, 15, 7, 0, 0, 0, time.Local)}
		pending := gedata.JobRow{JobID: "25", State: "hqw", Slots: 1}
		cfg := config.Default()
		cfg.Partitions = map[string]config.Partition{"mpi": {Queue: "all.q", PE: "smp"}}
		detail := gedata.JobInfo{Account: "projX", WorkDir: "/w", PE: "smp", Queues: []string{"all.q"},
			Submit: time.Date(2026, 9, 15, 6, 0, 0, 0, time.Local), Begin: time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local),
			Requests: map[string]gedata.ResourceRequest{"h_rt": {Amount: 60}, "mem_free": {Amount: 1 << 30}, "gpu": {Amount: 1}}}
		with := view{cfg: cfg, now: time.Now(),
			hosts:   map[string]gedata.JobAllocation{"23": {Hosts: []string{"a", "b"}}},
			details: map[string]gedata.JobInfo{"23": detail, "25": detail}}
		without := view{cfg: cfg, now: with.now}
		for verb := byte('A'); verb <= 'z'; verb++ {
			if headerTitle(verb) == "" {
				continue
			}
			uses := with.rowValue(verb, running) != without.rowValue(verb, running) ||
				with.rowValue(verb, pending) != without.rowValue(verb, pending)
			Expect(needsDetail(tokenize("%"+string(verb)))).To(Equal(uses),
				"verb %"+string(verb)+": the gate and the renderer disagree about whether it needs the detail")
		}
	})
})

// readEmptyDetail is a well-formed detail view that happens to carry no job --
// what qstat returns when the job it was asked about is no longer known.
const readEmptyDetail = `<?xml version='1.0'?>
<detailed_job_info><djob_info></djob_info></detailed_job_info>`

var _ = Describe("squeue default format [REQ-SQU-002]", func() {
	BeforeEach(func() {
		// Two days after the fixture's start time: TIME is a sized column, cut to
		// ten characters as SLURM cuts it, so the elapsed time must fit.
		clock = func() time.Time { return time.Date(2020, 1, 4, 3, 4, 5, 0, time.Local) }
		DeferCleanup(func() { clock = time.Now })
	})

	It("renders real TIME, NODES and NODELIST in the default output", func() {
		// The branch's headline claim, asserted where users actually read it: the
		// default eight-column format, not a hand-picked -o string.
		var out bytes.Buffer
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "23"}, &out, io.Discard)).To(Equal(0))
		fields := strings.Fields(out.String())
		Expect(fields).To(HaveLen(8))
		Expect(fields[0]).To(Equal("23"))
		Expect(fields[5]).To(MatchRegexp(`^\d+-\d\d:\d\d:\d\d$`), "TIME must be elapsed time, not 0:00")
		Expect(fields[6]).To(Equal("3"), "NODES must be the granted host count")
		Expect(fields[7]).To(Equal("ocs-worker2,ocs-master,ocs-worker1"))
	})

	It("shows 0:00 and no allocation for a pending job in the default output", func() {
		var out bytes.Buffer
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "25"}, &out, io.Discard)).To(Equal(0))
		// A pending job has no queue, so PARTITION renders empty and the columns
		// are counted from the right-hand end: TIME, NODES, NODELIST(REASON).
		fields := strings.Fields(out.String())
		Expect(fields[len(fields)-3]).To(Equal("0:00"))
		Expect(fields[len(fields)-1]).To(Equal("(None)"))
	})
})

var _ = Describe("squeue TIME column [REQ-SQU-002]", func() {
	It("shows how long a running job has been running", func() {
		row := gedata.JobRow{State: "r", Start: time.Date(2026, 9, 15, 7, 0, 0, 0, time.UTC)}
		now := time.Date(2026, 9, 15, 7, 2, 5, 0, time.UTC)
		Expect(squeueElapsed(row, now)).To(Equal("2:05"))
	})

	It("adds an hour field past an hour", func() {
		row := gedata.JobRow{State: "r", Start: time.Date(2026, 9, 15, 5, 0, 0, 0, time.UTC)}
		now := time.Date(2026, 9, 15, 7, 2, 5, 0, time.UTC)
		Expect(squeueElapsed(row, now)).To(Equal("2:02:05"))
	})

	It("adds a day field past a day", func() {
		row := gedata.JobRow{State: "r", Start: time.Date(2026, 9, 13, 5, 0, 0, 0, time.UTC)}
		now := time.Date(2026, 9, 15, 7, 2, 5, 0, time.UTC)
		Expect(squeueElapsed(row, now)).To(Equal("2-02:02:05"))
	})

	It("keeps counting for a suspended job, as SLURM does", func() {
		row := gedata.JobRow{State: "s", Start: time.Date(2026, 9, 15, 7, 0, 0, 0, time.UTC)}
		now := time.Date(2026, 9, 15, 7, 2, 5, 0, time.UTC)
		Expect(squeueElapsed(row, now)).To(Equal("2:05"))
	})

	It("keeps counting for a completing job", func() {
		row := gedata.JobRow{State: "dr", Start: time.Date(2026, 9, 15, 7, 0, 0, 0, time.UTC)}
		now := time.Date(2026, 9, 15, 7, 2, 5, 0, time.UTC)
		Expect(squeueElapsed(row, now)).To(Equal("2:05"))
	})

	It("shows 0:00 for a pending job, as SLURM does", func() {
		row := gedata.JobRow{State: "qw"}
		Expect(squeueElapsed(row, time.Now())).To(Equal("0:00"))
	})

	It("shows 0:00 rather than a negative time when clocks disagree", func() {
		row := gedata.JobRow{State: "r", Start: time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)}
		now := time.Date(2026, 9, 15, 7, 0, 0, 0, time.UTC)
		Expect(squeueElapsed(row, now)).To(Equal("0:00"))
	})
})

var _ = Describe("squeue options it does not translate [REQ-RUN-005]", func() {
	It("warns, steps over the value, and keeps the options it does translate", func() {
		opt, err := parse([]string{"-S", "P", "--start", "-i", "5", "-h", "-j", "4711"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.jobs).To(Equal([]string{"4711"}))
		Expect(opt.noHeader).To(BeTrue())
		Expect(opt.warnings).To(ConsistOf(
			ContainSubstring("-S ignored"), ContainSubstring("--start ignored"), ContainSubstring("-i ignored")))
	})
})

var _ = Describe("squeue array ids are SLURM's 0-based indices [SI-14]", func() {
	It("renders a pending range as SLURM does and finds an element inside it", func() {
		row := gedata.JobRow{JobID: "4720", State: "qw", TaskID: "1-10:1"}
		Expect(slurmID(row)).To(Equal("4720_[0-9]"))
		Expect(slurmID(gedata.JobRow{JobID: "4720", TaskID: "1-9:2"})).To(Equal("4720_[0-8:2]"))
		Expect(selectJobs([]gedata.JobRow{row}, []string{"4720_9"})).To(HaveLen(1))
		Expect(selectJobs([]gedata.JobRow{row}, []string{"4720_10"})).To(BeEmpty())
		Expect(selectJobs([]gedata.JobRow{row}, []string{"4720_x"})).To(BeEmpty())
	})
})

// liveRunner answers with the unedited OCS 9.1.6 captures in gedata's testdata
// (see jobdetail_test.go there for what each job is): the plain listing for
// qstat -xml -u, the detail view for qstat -xml -j. The listing also holds
// jobs of other users of the test cluster (sc_*), which the detail view does
// not cover: they show how a job without detail renders.
func liveRunner() *fake.Runner {
	plain, err := os.ReadFile("../../gedata/testdata/qstat_xml_squeue_9.1.6.xml")
	Expect(err).NotTo(HaveOccurred())
	detail, err := os.ReadFile("../../gedata/testdata/qstat_j_squeue_9.1.6.xml")
	Expect(err).NotTo(HaveOccurred())
	return &fake.Runner{Responder: func(_ string, args []string) fake.Response {
		if isDetailed(args) {
			return fake.Response{Stdout: detail}
		}
		return fake.Response{Stdout: plain}
	}}
}

// liveCfg has two partitions on all.q, told apart by their PE.
func liveCfg() *config.Config {
	c := config.Default()
	c.Partitions = map[string]config.Partition{
		"mpi":    {Queue: "all.q", PE: "smp"},
		"serial": {Queue: "all.q"},
	}
	return c
}

// squeueLines runs squeue against the live captures and returns its stdout
// lines and stderr.
func squeueLines(args ...string) ([]string, string) {
	var out, errOut bytes.Buffer
	Expect(run(liveRunner(), liveCfg(), args, &out, &errOut)).To(Equal(0), errOut.String())
	return strings.Split(strings.TrimRight(out.String(), "\n"), "\n"), errOut.String()
}

var _ = Describe("squeue against a live OCS 9.1.6 capture [REQ-SQU-002]", func() {
	BeforeEach(func() {
		// Shortly after the capture, before job 1371's -a start time.
		clock = func() time.Time { return time.Date(2026, 10, 12, 13, 45, 0, 0, time.Local) }
		DeferCleanup(func() { clock = time.Now })
	})

	DescribeTable("gives a pending job SLURM's reason from its own state",
		func(id, want string) {
			lines, _ := squeueLines("-h", "-j", id, "-o", "%i|%r|%R")
			Expect(lines).To(Equal([]string{want}))
		},
		Entry("a user hold", "1369", "1369|JobHeldUser|(JobHeldUser)"),
		Entry("an operator hold", "1393", "1393|JobHeldAdmin|(JobHeldAdmin)"),
		Entry("an unfinished predecessor", "1395", "1395|Dependency|(Dependency)"),
		Entry("a start time to come", "1371", "1371|BeginTime|(BeginTime)"),
		Entry("a job Grid Engine failed to start (Eqw)", "1394",
			"1394|launch failed requeued held|(launch failed requeued held)"),
		Entry("a held pending range", "1372", "1372_[0-4]|JobHeldUser|(JobHeldUser)"),
		Entry("a running job", "1397", "1397|None|ocs-worker1"),
	)

	It("says None, not a guess, when the scheduler keeps no job information", func() {
		lines, _ := squeueLines("-h", "-j", "1378", "-o", "%i %r")
		Expect(lines).To(Equal([]string{"1378 None"}))
	})

	It("lists the default format with two qstat calls, whatever the job count", func() {
		r := liveRunner()
		Expect(run(r, liveCfg(), nil, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls).To(HaveLen(2))
	})

	It("shows a pending array as one range row, and one row per element with -r", func() {
		lines, _ := squeueLines("-h", "-j", "1372,1398", "-o", "%i %t")
		Expect(lines).To(Equal([]string{"1398_1 R", "1398_2 R", "1372_[0-4] PD", "1398_[0,3] PD"}))
		lines, _ = squeueLines("-h", "-r", "-j", "1372,1398", "-o", "%i %t")
		Expect(lines).To(Equal([]string{"1398_1 R", "1398_2 R",
			"1372_0 PD", "1372_1 PD", "1372_2 PD", "1372_3 PD", "1372_4 PD", "1398_0 PD", "1398_3 PD"}))
	})

	It("narrows a pending range to the element -j names", func() {
		lines, _ := squeueLines("-h", "-j", "1398_3,1369", "-o", "%i")
		Expect(lines).To(Equal([]string{"1369", "1398_3"}))
	})

	DescribeTable("filters, and every filter narrows the others",
		func(args []string, want []string) {
			lines, _ := squeueLines(append([]string{"-h", "-o", "%i"}, args...)...)
			Expect(lines).To(ConsistOf(want))
		},
		Entry("by state name", []string{"-t", "RUNNING"},
			[]string{"1396_0", "1396_1", "1396_2", "1397", "1399", "1400", "1398_1", "1398_2"}),
		Entry("by compact state, against a running-only name", []string{"-t", "pd", "-n", "sqF_arrrun"}, []string{""}),
		Entry("by name", []string{"-n", "sqC_dep,sqB_held"}, []string{"1369", "1395"}),
		Entry("by account", []string{"-A", "projX"}, []string{"1394"}),
		Entry("by partition, the PE telling two on one queue apart", []string{"-p", "mpi"}, []string{"1397"}),
		Entry("by partition and state", []string{"-p", "serial", "-t", "R", "-n", "sqF_arrrun"},
			[]string{"1396_0", "1396_1", "1396_2"}),
		Entry("by node", []string{"-w", "ocs-worker1"}, []string{"1396_0", "1397"}),
		Entry("by a hostlist", []string{"-w", "ocs-worker[1-2]", "-n", "sqF_arrrun,sqI_arrsplit"},
			[]string{"1396_0", "1396_2", "1398_1"}),
		Entry("by every state", []string{"-t", "all", "-n", "sqB_held"}, []string{"1369"}),
	)

	It("refuses a state SLURM does not know", func() {
		var errOut bytes.Buffer
		Expect(run(liveRunner(), liveCfg(), []string{"-t", "WAITING"}, io.Discard, &errOut)).To(Equal(1))
		Expect(errOut.String()).To(ContainSubstring("invalid job state specified: WAITING"))
	})

	It("renders SLURM's long format, cutting headers and values to their fields", func() {
		lines, _ := squeueLines("-l", "-j", "1393,1397")
		Expect(lines).To(Equal([]string{
			"             JOBID PARTITION     NAME     USER    STATE       TIME TIME_LIMI  NODES NODELIST(REASON)",
			"              1397       mpi   sqG_pe gridware  RUNNING       6:52     30:00      1 ocs-worker1",
			"              1393           sqH_opho gridware  PENDING       0:00 3-18:00:0      1 (JobHeldAdmin)",
		}))
	})

	It("renders the detail columns", func() {
		lines, _ := squeueLines("-h", "-j", "1393,1394,1397,1369", "-o", "%i|%l|%S|%V|%a|%m|%b|%Z|%c|%p|%Q")
		Expect(lines).To(Equal([]string{
			"1397|30:00|2026-10-12T13:38:07|2026-10-12T13:38:07|sge|0|N/A|/home/gridware|2|0.60500000|2598455213",
			"1394|1:00:00|N/A|2026-10-12T13:38:07|projX|0|N/A|/tmp/sqdev-out|1|0.50500000|2168958484",
			"1369|UNLIMITED|N/A|2026-10-12T13:23:18|sge|0|N/A|/home/gridware|1|0.00000000|0",
			"1393|3-18:00:00|N/A|2026-10-12T13:37:45|sge|2G|N/A|/tmp/sqdev-out|1|0.00000000|0",
		}))
	})

	It("shows a pending job's -a time as its start time", func() {
		lines, _ := squeueLines("-h", "-j", "1371", "-o", "%S")
		Expect(lines).To(Equal([]string{"2026-10-12T15:23:00"}))
	})

	It("renders -O fields left-justified in 20 characters unless sized", func() {
		lines, _ := squeueLines("-j", "1394", "-O", "JobID,statecompact:4,TimeLimit:.9,Account:8|,Reason:30")
		Expect(lines).To(Equal([]string{
			"JOBID               ST  TIME_LIMIACCOUNT |REASON                        ",
			"1394                PD    1:00:00projX   |launch failed requeued held   ",
		}))
	})

	It("warns about an -O field it does not render and leaves it out", func() {
		lines, errOut := squeueLines("-h", "-j", "1369", "-O", "JobID:6,Licenses,Name:8")
		Expect(lines).To(Equal([]string{"1369  sqB_held"}))
		Expect(errOut).To(ContainSubstring("format field Licenses not supported"))
	})

	It("renders rows without detail, from other users' jobs, as unknown", func() {
		lines, _ := squeueLines("-h", "-j", "1399", "-o", "%i|%a|%l|%Z|%N")
		Expect(lines).To(Equal([]string{"1399|(null)|N/A|(null)|ocs-worker2"}))
	})
})

var _ = Describe("squeue options and environment [REQ-SQU-002]", func() {
	var env map[string]string
	BeforeEach(func() {
		env = map[string]string{}
		getenv = func(k string) string { return env[k] }
		currentUser = func() string { return "alice" }
		DeferCleanup(func() { getenv, currentUser = os.Getenv, loginName })
	})

	userArg := func(r *fake.Runner) string {
		Expect(r.Calls).NotTo(BeEmpty())
		return r.Calls[0].Args[len(r.Calls[0].Args)-1]
	}

	It("passes a -u list to qstat as one comma list", func() {
		r := fakeQstat()
		Expect(run(r, testCfg(), []string{"-u", "alice,bob", "-o", "%i"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(userArg(r)).To(Equal("alice,bob"))
	})

	It("lists the caller's jobs with --me", func() {
		r := fakeQstat()
		Expect(run(r, testCfg(), []string{"--me", "-o", "%i"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(userArg(r)).To(Equal("alice"))
	})

	It("reads SQUEUE_USERS, and an option beats it", func() {
		env["SQUEUE_USERS"] = "carol"
		r := fakeQstat()
		Expect(run(r, testCfg(), []string{"-o", "%i"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(userArg(r)).To(Equal("carol"))
		r = fakeQstat()
		Expect(run(r, testCfg(), []string{"-u", "dave", "-o", "%i"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(userArg(r)).To(Equal("dave"))
	})

	DescribeTable("resolves the output format",
		func(vars map[string]string, args []string, header string) {
			for k, v := range vars {
				env[k] = v
			}
			var out bytes.Buffer
			Expect(run(fakeQstat(), testCfg(), args, &out, io.Discard)).To(Equal(0))
			Expect(strings.SplitN(out.String(), "\n", 2)[0]).To(Equal(header))
		},
		Entry("SQUEUE_FORMAT", map[string]string{"SQUEUE_FORMAT": "%i %t"}, nil, "JOBID ST"),
		Entry("SQUEUE_FORMAT2 over SQUEUE_FORMAT",
			map[string]string{"SQUEUE_FORMAT": "%i %t", "SQUEUE_FORMAT2": "JobID:6,StateCompact:3"}, nil, "JOBID ST "),
		Entry("-o over the variables", map[string]string{"SQUEUE_FORMAT2": "JobID"}, []string{"-o", "%t"}, "ST"),
		Entry("-l over the variables", map[string]string{"SQUEUE_FORMAT": "%t"}, []string{"-l"},
			"             JOBID PARTITION     NAME     USER    STATE       TIME TIME_LIMI  NODES NODELIST(REASON)"),
		Entry("-o over -l", nil, []string{"-o", "%t", "-l"}, "ST"),
	)

	It("reads SQUEUE_STATES, SQUEUE_PARTITION and SQUEUE_ACCOUNT, and options beat them", func() {
		env["SQUEUE_STATES"] = "R"
		env["SQUEUE_PARTITION"] = "gpu"
		env["SQUEUE_ACCOUNT"] = "projX"
		env["SQUEUE_SORT"] = "-t" // read by SLURM, ignored here without a word
		opt, err := parse(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.states).To(Equal(map[string]bool{"R": true}))
		Expect(opt.partitions).To(Equal(map[string]bool{"gpu": true}))
		Expect(opt.accounts).To(Equal(map[string]bool{"projX": true}))
		Expect(opt.warnings).To(BeEmpty())
		opt, err = parse([]string{"-t", "PD,CG", "-p", "batch", "-A", "a,b"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.states).To(Equal(map[string]bool{"PD": true, "CG": true}))
		Expect(opt.partitions).To(Equal(map[string]bool{"batch": true}))
		Expect(opt.accounts).To(Equal(map[string]bool{"a": true, "b": true}))
	})

	It("unpacks a bundle of flags", func() {
		opt, err := parse([]string{"-hlr"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.noHeader && opt.long && opt.array).To(BeTrue())
	})

	It("takes every form of a valued option", func() {
		opt, err := parse([]string{"-tPD", "--partition=gpu", "--jobs", "1,2_3", "--name=a"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.states).To(HaveKey("PD"))
		Expect(opt.partitions).To(HaveKey("gpu"))
		Expect(opt.jobs).To(Equal([]string{"1", "2_3"}))
		Expect(opt.names).To(HaveKey("a"))
	})

	It("lists every job for a bare -j, as SLURM does", func() {
		opt, err := parse([]string{"-j", "-h"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.jobs).To(BeEmpty())
		Expect(opt.noHeader).To(BeTrue())
	})

	It("refuses a malformed node list", func() {
		_, err := parse([]string{"-w", "node[1-"})
		Expect(err).To(MatchError(ContainSubstring("invalid node name")))
	})
})

// ocs9010Runner answers like the live OCS 9.0.10 capture of a job held with
// sbatch -H, whose `qstat -xml -j` leaves the hold ranges out.
func ocs9010Runner() *fake.Runner {
	read := func(f string) []byte {
		data, err := os.ReadFile("../../gedata/testdata/" + f)
		Expect(err).NotTo(HaveOccurred())
		return data
	}
	return &fake.Runner{Responder: func(_ string, args []string) fake.Response {
		switch strings.Join(args, " ") {
		case "-xml -u *":
			return fake.Response{Stdout: read("qstat_xml_held_9.0.10.xml")}
		case "-xml -j 1":
			return fake.Response{Stdout: read("qstat_j_held_9.0.10.xml")}
		case "-xml -u * -s hu":
			return fake.Response{Stdout: read("qstat_s_hu_9.0.10.xml")}
		case "-xml -u * -s ho", "-xml -u * -s hs":
			return fake.Response{Stdout: read("qstat_s_ho_empty_9.0.10.xml")}
		}
		return fake.Response{Exit: 1, Stderr: []byte("unexpected qstat " + strings.Join(args, " "))}
	}}
}

var _ = Describe("squeue pending reasons on OCS 9.0 [gedata.HoldKinds]", func() {
	It("reads a user hold from qstat -s hu when the job detail leaves it out", func() {
		r := ocs9010Runner()
		var out, errOut bytes.Buffer
		Expect(run(r, testCfg(), []string{"-h", "-u", "*", "-o", "%i %r"}, &out, &errOut)).To(Equal(0), errOut.String())
		Expect(strings.TrimSpace(out.String())).To(Equal("1 JobHeldUser"))
		Expect(errOut.String()).To(BeEmpty())
	})

	It("asks nothing more when no column needs a reason", func() {
		r := ocs9010Runner()
		Expect(run(r, testCfg(), []string{"-h", "-u", "*", "-o", "%i"}, io.Discard, io.Discard)).To(Equal(0))
		for _, c := range r.Calls {
			Expect(c.Args).NotTo(ContainElement("-s"))
		}
	})
})
