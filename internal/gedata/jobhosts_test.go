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
	parse := func(fixture string) map[string]gedata.JobAllocation {
		hosts, err := gedata.ParseJobAllocationsXML(readFixture(fixture))
		Expect(err).NotTo(HaveOccurred())
		return hosts
	}

	It("collects every host a multi-node job occupies, in grant order", func() {
		Expect(parse("qstat_j_granted.xml")["146"].Hosts).To(Equal(
			[]string{"ocs-master", "ocs-worker1", "ocs-worker2"}))
	})

	It("reads a job whose name contains a space, and leaves its neighbours alone", func() {
		// The defect that forced the move off `qstat -g t`: that view splits on
		// whitespace, so this job's name shifted every column, the record was
		// dropped, and its continuation lines overwrote the PREVIOUS job's host.
		// Here the name is an element, so it cannot shift anything.
		hosts := parse("qstat_j_granted.xml")
		Expect(hosts["147"].Hosts).To(Equal([]string{"ocs-worker1", "ocs-worker2"}))
		Expect(hosts["146"].Hosts).To(Equal([]string{"ocs-master", "ocs-worker1", "ocs-worker2"}),
			"the job before the spaced name must be untouched")
	})

	It("keys each array element separately", func() {
		hosts := parse("qstat_j_granted.xml")
		Expect(hosts["148_1"].Hosts).To(Equal([]string{"ocs-worker1", "ocs-worker2"}))
		Expect(hosts["148_2"].Hosts).To(Equal([]string{"ocs-master", "ocs-worker1"}))
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
		Expect(parse("qstat_j_two_queues_one_host.xml")["149"].Hosts).To(Equal([]string{"ocs-worker2"}))
	})

	It("keeps a queue instance longer than thirty characters intact", func() {
		// Same job, read through its queue names: the text view truncated this
		// instance to "long-queue-name-to-exceed-thir" and lost the host with it.
		Expect(parse("qstat_j_two_queues_one_host.xml")["149"].Hosts).To(ConsistOf("ocs-worker2"))
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
		hosts, err := gedata.ParseJobAllocationsXML(fqdnGrant)
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts["31"].Hosts).To(Equal([]string{"shimval-g1"}))
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
		hosts, err := gedata.ParseJobAllocationsXML(mixed)
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts["33"].Hosts).To(Equal([]string{"node001"}))
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
		hosts, err := gedata.ParseJobAllocationsXML(noHostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts["32"].Hosts).To(Equal([]string{"node001"}))
	})

	It("treats a job qstat does not know as an empty answer, not a failure", func() {
		// qstat answers an unmatched selector with an <unknown_jobs> document whose
		// entries are wrapped in a literal "<>" element -- which is not well-formed
		// XML, so this must be recognised before any unmarshalling is attempted.
		hosts, err := gedata.ParseJobAllocationsXML(readFixture("qstat_j_unknown.xml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(hosts).To(BeEmpty())
	})

	It("reports unreadable output rather than returning an empty map", func() {
		_, err := gedata.ParseJobAllocationsXML([]byte("<detailed_job_info><djob_info>"))
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
			hosts, err := gedata.ParseJobAllocationsXML(data)
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts[jobID].Hosts).To(Equal([]string{"ocs-worker1"}))
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
			_, err := gedata.JobAllocations(context.Background(), r, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Calls).To(HaveLen(1))
			Expect(r.Calls[0].Name).To(Equal("qstat"))
			Expect(r.Calls[0].Args).To(Equal([]string{"-xml", "-j", "*"}))
		})

		It("narrows to one job, by its base id, for an array element", func() {
			// qstat -j takes a job id, not a SLURM array element id, and narrowing
			// is the only narrowing available: qstat -j ignores -u.
			r := record(fake.Response{Stdout: readFixture("qstat_j_granted.xml")})
			_, err := gedata.JobAllocations(context.Background(), r, "148_2")
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Calls[0].Args).To(Equal([]string{"-xml", "-j", "148"}))
		})

		It("surfaces a non-zero exit with what qstat said", func() {
			r := record(fake.Response{Exit: 1, Stderr: []byte("qmaster unreachable")})
			_, err := gedata.JobAllocations(context.Background(), r, "")
			Expect(err).To(MatchError(ContainSubstring("qmaster unreachable")))
		})

		It("surfaces a launch failure", func() {
			r := record(fake.Response{Err: errors.New("exec: qstat not found")})
			_, err := gedata.JobAllocations(context.Background(), r, "")
			Expect(err).To(MatchError(ContainSubstring("qstat not found")))
		})

		It("keeps the diagnosis when qstat exits zero but writes to stderr", func() {
			// The stderr text is the only explanation of why stdout is unreadable;
			// discarding it leaves the caller to fall back with no reason to give.
			r := record(fake.Response{Stdout: []byte("not xml at all"), Stderr: []byte("denied: no read access")})
			_, err := gedata.JobAllocations(context.Background(), r, "")
			Expect(err).To(MatchError(ContainSubstring("denied: no read access")))
		})
	})
})

var _ = Describe("hot spares in the job view [sbatch --x-spares]", func() {
	// Element names as captured live from OCS 9.1.6 qstat -xml -j.
	doc := func(env, context string) []byte {
		return []byte(`<?xml version='1.0'?><detailed_job_info><djob_info><element>
<JB_job_number>500</JB_job_number>
<JB_env_list>` + env + `</JB_env_list>
<JB_context>` + context + `</JB_context>
<JB_ja_tasks><element><JAT_task_number>1</JAT_task_number><JAT_granted_destin_identifier_list>
<element><JG_qname>all.q@n1</JG_qname><JG_qhostname>n1</JG_qhostname></element>
<element><JG_qname>all.q@n2</JG_qname><JG_qhostname>n2</JG_qhostname></element>
<element><JG_qname>all.q@n3</JG_qname><JG_qhostname>n3</JG_qhostname></element>
<element><JG_qname>all.q@n4</JG_qname><JG_qhostname>n4</JG_qhostname></element>
</JAT_granted_destin_identifier_list></element></JB_ja_tasks>
</element></djob_info></detailed_job_info>`)
	}
	spares := func(k string) string {
		return `<job_sublist><VA_variable>SLURM_SHIM_SPARES</VA_variable><VA_value>` + k + `</VA_value></job_sublist>`
	}
	swaps := func(v string) string {
		return `<context_list><VA_variable>shim.swaps</VA_variable><VA_value>` + v + `</VA_value></context_list>`
	}

	It("keeps the spares out of the job's hosts", func() {
		got, err := gedata.ParseJobAllocationsXML(doc(spares("2"), ""))
		Expect(err).NotTo(HaveOccurred())
		Expect(got["500"].Hosts).To(Equal([]string{"n1", "n2"}))
		Expect(got["500"].Spares).To(Equal([]string{"n3", "n4"}))
	})

	It("applies the swaps srun recorded, in order", func() {
		got, err := gedata.ParseJobAllocationsXML(doc(spares("2"), swaps("n2:n3+n3:n4")))
		Expect(err).NotTo(HaveOccurred())
		Expect(got["500"].Hosts).To(Equal([]string{"n1", "n4"}))
		Expect(got["500"].Spares).To(BeEmpty())
		Expect(got["500"].Lost).To(Equal([]string{"n2", "n3"}))
	})

	It("leaves a job without spares unchanged", func() {
		got, err := gedata.ParseJobAllocationsXML(doc("", ""))
		Expect(err).NotTo(HaveOccurred())
		Expect(got["500"].Hosts).To(HaveLen(4))
		Expect(got["500"].Spares).To(BeEmpty())
	})

	// todo 152: a requeued job may keep the previous run's swaps when ClearSwaps
	// failed; they name hosts this grant does not hold and must not be applied.
	It("ignores a swap whose lost host is not one of the job's nodes", func() {
		got, err := gedata.ParseJobAllocationsXML(doc(spares("2"), swaps("x9:n3+n2:n4")))
		Expect(err).NotTo(HaveOccurred())
		Expect(got["500"].Hosts).To(Equal([]string{"n1", "n4"}))
		Expect(got["500"].Spares).To(Equal([]string{"n3"}))
		Expect(got["500"].Lost).To(Equal([]string{"n2"}))
	})

	It("ignores a swap whose spare is not an unused spare of this grant", func() {
		got, err := gedata.ParseJobAllocationsXML(doc(spares("2"), swaps("n2:x9+n2:n1")))
		Expect(err).NotTo(HaveOccurred())
		Expect(got["500"].Hosts).To(Equal([]string{"n1", "n2"}))
		Expect(got["500"].Spares).To(Equal([]string{"n3", "n4"}))
		Expect(got["500"].Lost).To(BeEmpty())
	})

	It("skips a malformed swap pair and applies the rest", func() {
		got, err := gedata.ParseJobAllocationsXML(doc(spares("2"), swaps("n2+:n3+n1n3+n2:n3")))
		Expect(err).NotTo(HaveOccurred())
		Expect(got["500"].Hosts).To(Equal([]string{"n1", "n3"}))
		Expect(got["500"].Spares).To(Equal([]string{"n4"}))
		Expect(got["500"].Lost).To(Equal([]string{"n2"}))
	})

	DescribeTable("keeps every host when the spare count cannot apply, as the fabricator does",
		func(k string) {
			got, err := gedata.ParseJobAllocationsXML(doc(spares(k), swaps("n2:n4")))
			Expect(err).NotTo(HaveOccurred())
			Expect(got["500"].Hosts).To(Equal([]string{"n1", "n2", "n3", "n4"}))
			Expect(got["500"].Spares).To(BeEmpty())
			Expect(got["500"].Lost).To(BeEmpty())
		},
		Entry("k equal to the grant", "4"),
		Entry("k larger than the grant", "9"),
		Entry("a non-numeric k", "two"),
		Entry("k of 0, sent over an inherited count", "0"),
	)

	It("reports no allocation for a pending spares job", func() {
		pending := []byte(`<?xml version='1.0'?><detailed_job_info><djob_info><element>
<JB_job_number>501</JB_job_number>
<JB_env_list>` + spares("1") + `</JB_env_list>
</element></djob_info></detailed_job_info>`)
		got, err := gedata.ParseJobAllocationsXML(pending)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(HaveKey("501"))
	})
})

var _ = Describe("recording hot-spare swaps in the job context", func() {
	ok := func() *fake.Runner {
		return &fake.Runner{Responder: func(string, []string) fake.Response { return fake.Response{} }}
	}

	It("stores the swaps with qalter -ac, '+'-joined", func() {
		r := ok()
		Expect(gedata.RecordSwaps(context.Background(), r, "4711", []string{"a:b", "c:d"})).To(Succeed())
		Expect(r.Calls).To(HaveLen(1))
		Expect(r.Calls[0].Name).To(Equal("qalter"))
		Expect(r.Calls[0].Args).To(Equal([]string{"-ac", "shim.swaps=a:b+c:d", "4711"}))
	})

	It("clears them with qalter -dc", func() {
		r := ok()
		Expect(gedata.ClearSwaps(context.Background(), r, "4711")).To(Succeed())
		Expect(r.Calls[0].Name).To(Equal("qalter"))
		Expect(r.Calls[0].Args).To(Equal([]string{"-dc", "shim.swaps", "4711"}))
	})

	It("reports a non-zero qalter exit with what qalter said", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("denied: host \"n1\" is no submit host\n")}
		}}
		Expect(gedata.RecordSwaps(context.Background(), r, "4711", []string{"a:b"})).To(
			MatchError(ContainSubstring("qalter: exit 1: denied")))
		Expect(gedata.ClearSwaps(context.Background(), r, "4711")).To(
			MatchError(ContainSubstring("no submit host")))
	})

	It("reports a qalter that could not run", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Err: errors.New("exec: qalter not found")}
		}}
		Expect(gedata.ClearSwaps(context.Background(), r, "4711")).To(MatchError(ContainSubstring("qalter not found")))
	})
})
