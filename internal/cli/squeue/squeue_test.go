package squeue

import (
	"bytes"
	"io"
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
		Expect(rows).To(ContainElement("4713_2 PD")) // array task rendered 4713_2
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

	It("filters to a single array task", func() {
		var out bytes.Buffer
		Expect(run(fakeQstat(), testCfg(), []string{"-h", "-j", "4713_2", "-o", "%i"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("4713_2"))
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
		v := view{cfg: testCfg(), hosts: map[string][]string{"23": hosts}, now: time.Now()}
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
		Expect(run(twoViewQstat(), testCfg(), []string{"-h", "-j", "24_2", "-o", "%i %D %N"}, &out, io.Discard)).To(Equal(0))
		Expect(strings.TrimSpace(out.String())).To(Equal("24_2 2 ocs-worker[2,1]"))
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
		Expect(run(r, testCfg(), []string{"-h", "-j", "24_2", "-o", "%i %N"}, &out, io.Discard)).To(Equal(0))
		var detailed [][]string
		for _, c := range r.Calls {
			if isDetailed(c.Args) {
				detailed = append(detailed, c.Args)
			}
		}
		Expect(detailed).To(Equal([][]string{{"-xml", "-j", "24"}}))
	})

	It("asks for every job when the listing is not narrowed", func() {
		r := twoViewQstat()
		var out bytes.Buffer
		Expect(run(r, testCfg(), []string{"-h", "-o", "%i %N"}, &out, io.Discard)).To(Equal(0))
		var detailed [][]string
		for _, c := range r.Calls {
			if isDetailed(c.Args) {
				detailed = append(detailed, c.Args)
			}
		}
		Expect(detailed).To(Equal([][]string{{"-xml", "-j", "*"}}))
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
		Expect(errOut.String()).To(ContainSubstring("node list unavailable"))
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
		// needsHosts lists the verbs that trigger the second query and rowValue
		// reads the hosts for some set of verbs. If they drift apart, a node column
		// is either always empty or costs a query nothing reads.
		row := gedata.JobRow{JobID: "23", State: "r", Queue: "all.q@ocs-master", Slots: 4}
		withHosts := view{cfg: testCfg(), hosts: map[string][]string{"23": {"a", "b"}}, now: time.Now()}
		without := view{cfg: testCfg(), now: time.Now()}
		for verb := byte('A'); verb <= 'z'; verb++ {
			if headerTitle(verb) == "" {
				continue
			}
			uses := withHosts.rowValue(verb, row) != without.rowValue(verb, row)
			Expect(needsHosts("%"+string(verb))).To(Equal(uses),
				"verb %"+string(verb)+": the gate and the renderer disagree about whether it needs hosts")
		}
	})
})

// readEmptyDetail is a well-formed detail view that happens to carry no job --
// what qstat returns when the job it was asked about is no longer known.
const readEmptyDetail = `<?xml version='1.0'?>
<detailed_job_info><djob_info></djob_info></detailed_job_info>`

var _ = Describe("squeue default format [REQ-SQU-002]", func() {
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
