package gedata_test

import (
	"context"
	"errors"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// The fixtures are unedited `qstat -xml -j` captures from a live OCS 9.1.5
// cluster.
//
// qstat_j_granted.xml holds four jobs chosen to cover the shapes that matter:
//
//	146 "multi"  -pe make 4   granted on ocs-master, ocs-worker1, ocs-worker2
//	147 "my job" -pe make 2   a job NAME CONTAINING A SPACE, on worker1+worker2
//	148 "arr"    -t 1-2       an array: task 1 and task 2 on different hosts
//	145 "pend"   -pe make 99  pending, so it carries no task element at all
//
// qstat_j_two_queues_one_host.xml holds job 149, granted two slots in all.q and
// two more in a queue whose instance name is 52 characters long -- both on
// ocs-worker2. In the text view that same job printed the queue instance as
// "long-queue-name-to-exceed-thir", with the "@host" cut off entirely.
var _ = Describe("granted exec hosts per job [REQ-SQU-002]", func() {
	parse := func(fixture string) map[string][]string {
		hosts, err := gedata.ParseJobHostsXML(readFixture(fixture))
		Expect(err).NotTo(HaveOccurred())
		return hosts
	}

	It("collects every host a multi-node job occupies, in grant order", func() {
		Expect(parse("qstat_j_granted.xml")["146"]).To(Equal(
			[]string{"ocs-master", "ocs-worker1", "ocs-worker2"}))
	})

	It("reads a job whose name contains a space, and leaves its neighbours alone", func() {
		// The defect that forced the move off `qstat -g t`: that view splits on
		// whitespace, so this job's name shifted every column, the record was
		// dropped, and its continuation lines overwrote the PREVIOUS job's host.
		// Here the name is an element, so it cannot shift anything.
		hosts := parse("qstat_j_granted.xml")
		Expect(hosts["147"]).To(Equal([]string{"ocs-worker1", "ocs-worker2"}))
		Expect(hosts["146"]).To(Equal([]string{"ocs-master", "ocs-worker1", "ocs-worker2"}),
			"the job before the spaced name must be untouched")
	})

	It("keys each array element separately", func() {
		hosts := parse("qstat_j_granted.xml")
		Expect(hosts["148_1"]).To(Equal([]string{"ocs-worker1", "ocs-worker2"}))
		Expect(hosts["148_2"]).To(Equal([]string{"ocs-master", "ocs-worker1"}))
		Expect(hosts).NotTo(HaveKey("148"),
			"an array with several running tasks has no allocation of its own")
	})

	It("keys a job that is not an array by its bare job id", func() {
		Expect(parse("qstat_j_granted.xml")).To(HaveKey("147"))
	})

	It("reports no hosts for a job that has not started", func() {
		Expect(parse("qstat_j_granted.xml")).NotTo(HaveKey("145"))
	})

	It("counts a host once when a job holds slots in two queues on it", func() {
		// Two queues on one host is an ordinary Grid Engine configuration, and it
		// is one node, not two. Under the XML view each grant is its own element,
		// so the duplicate really does arrive here.
		Expect(parse("qstat_j_two_queues_one_host.xml")["149"]).To(Equal([]string{"ocs-worker2"}))
	})

	It("keeps a queue instance longer than thirty characters intact", func() {
		// Same job, read through its queue names: the text view truncated this
		// instance to "long-queue-name-to-exceed-thir" and lost the host with it.
		Expect(parse("qstat_j_two_queues_one_host.xml")["149"]).To(ConsistOf("ocs-worker2"))
	})

	It("reduces a fully qualified granted host to the short name", func() {
		// Caught by the e2e suite on a GCP cluster, where Grid Engine had
		// registered its hosts by fully qualified name: BOTH JG_qhostname and the
		// queue instance read the long form, while the job's own
		// SLURM_JOB_NODELIST read the short one, because the layout reduces every
		// PE_HOSTFILE host. squeue named hosts in a form the job never uses.
		fqdnGrant := []byte(`<?xml version='1.0'?>
<detailed_job_info><djob_info><element>
  <JB_job_number>31</JB_job_number>
  <JB_ja_tasks><element>
    <JAT_task_number>1</JAT_task_number>
    <JAT_granted_destin_identifier_list>
      <element>
        <JG_qname>all.q@shimval-g1.us-central1-b.c.example.internal</JG_qname>
        <JG_qhostname>shimval-g1.us-central1-b.c.example.internal</JG_qhostname>
      </element>
    </JAT_granted_destin_identifier_list>
  </element></JB_ja_tasks>
</element></djob_info></detailed_job_info>`)
		hosts, err := gedata.ParseJobHostsXML(fqdnGrant)
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts["31"]).To(Equal([]string{"shimval-g1"}))
	})

	It("counts one host when two queues on it differ only past the domain", func() {
		// Shortening must happen BEFORE de-duplication, or the same node arrives
		// twice and the node count is wrong.
		mixed := []byte(`<?xml version='1.0'?>
<detailed_job_info><djob_info><element>
  <JB_job_number>33</JB_job_number>
  <JB_ja_tasks><element>
    <JAT_task_number>1</JAT_task_number>
    <JAT_granted_destin_identifier_list>
      <element><JG_qhostname>node001.example.internal</JG_qhostname></element>
      <element><JG_qhostname>node001</JG_qhostname></element>
    </JAT_granted_destin_identifier_list>
  </element></JB_ja_tasks>
</element></djob_info></detailed_job_info>`)
		hosts, err := gedata.ParseJobHostsXML(mixed)
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts["33"]).To(Equal([]string{"node001"}))
	})

	It("falls back to the queue instance when there is no hostname field", func() {
		noHostname := []byte(`<?xml version='1.0'?>
<detailed_job_info><djob_info><element>
  <JB_job_number>32</JB_job_number>
  <JB_ja_tasks><element>
    <JAT_task_number>1</JAT_task_number>
    <JAT_granted_destin_identifier_list>
      <element><JG_qname>all.q@node001.example.internal</JG_qname></element>
    </JAT_granted_destin_identifier_list>
  </element></JB_ja_tasks>
</element></djob_info></detailed_job_info>`)
		hosts, err := gedata.ParseJobHostsXML(noHostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts["32"]).To(Equal([]string{"node001"}))
	})

	It("treats a job qstat does not know as an empty answer, not a failure", func() {
		// qstat answers an unmatched selector with an <unknown_jobs> document whose
		// entries are wrapped in a literal "<>" element -- which is not well-formed
		// XML, so this must be recognised before any unmarshalling is attempted.
		hosts, err := gedata.ParseJobHostsXML(readFixture("qstat_j_unknown.xml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts).To(BeEmpty())
	})

	It("reports unreadable output rather than returning an empty map", func() {
		_, err := gedata.ParseJobHostsXML([]byte("<detailed_job_info><djob_info>"))
		Expect(err).To(HaveOccurred())
	})

	DescribeTable("reads the granted hosts from every supported OCS release",
		func(version, jobID string) {
			// The e2e suite captures `qstat -xml -j` from each release it supports.
			// JAT_granted_destin_identifier_list, JG_qhostname and JAT_task_number
			// are present and identically shaped in all of them, which is what makes
			// this parser safe across the version matrix rather than pinned to the
			// cluster it was written against.
			data, err := os.ReadFile("../../test/e2e/fixtures/" + version + "/qstat-xml-j.xml")
			Expect(err).NotTo(HaveOccurred())
			hosts, err := gedata.ParseJobHostsXML(data)
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts[jobID]).To(Equal([]string{"ocs-worker1"}))
		},
		Entry("OCS 9.0.10", "9.0.10", "7"),
		Entry("OCS 9.1.4", "9.1.4", "7"),
		Entry("OCS 9.1.5", "9.1.5", "17"),
	)

	Describe("the qstat invocation", func() {
		record := func(resp fake.Response) *fake.Runner {
			return &fake.Runner{Responder: func(string, []string) fake.Response { return resp }}
		}

		It("asks for every job when no job was named", func() {
			r := record(fake.Response{Stdout: readFixture("qstat_j_granted.xml")})
			_, err := gedata.JobHosts(context.Background(), r, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Calls).To(HaveLen(1))
			Expect(r.Calls[0].Name).To(Equal("qstat"))
			Expect(r.Calls[0].Args).To(Equal([]string{"-xml", "-j", "*"}))
		})

		It("narrows to one job, by its base id, for an array element", func() {
			// qstat -j takes a job id, not a SLURM array element id, and narrowing
			// is the only narrowing available: qstat -j ignores -u.
			r := record(fake.Response{Stdout: readFixture("qstat_j_granted.xml")})
			_, err := gedata.JobHosts(context.Background(), r, "148_2")
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Calls[0].Args).To(Equal([]string{"-xml", "-j", "148"}))
		})

		It("surfaces a non-zero exit with what qstat said", func() {
			r := record(fake.Response{Exit: 1, Stderr: []byte("qmaster unreachable")})
			_, err := gedata.JobHosts(context.Background(), r, "")
			Expect(err).To(MatchError(ContainSubstring("qmaster unreachable")))
		})

		It("surfaces a launch failure", func() {
			r := record(fake.Response{Err: errors.New("exec: qstat not found")})
			_, err := gedata.JobHosts(context.Background(), r, "")
			Expect(err).To(MatchError(ContainSubstring("qstat not found")))
		})

		It("keeps the diagnosis when qstat exits zero but writes to stderr", func() {
			// The stderr text is the only explanation of why stdout is unreadable;
			// discarding it leaves the caller to fall back with no reason to give.
			r := record(fake.Response{Stdout: []byte("not xml at all"), Stderr: []byte("denied: no read access")})
			_, err := gedata.JobHosts(context.Background(), r, "")
			Expect(err).To(MatchError(ContainSubstring("denied: no read access")))
		})
	})
})
