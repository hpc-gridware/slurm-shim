package scontrol

import (
	"bytes"
	"io"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// pendingCluster is liveCluster with every job not yet started: the running
// fixtures carry the resource lists the merge is tested against, and only a
// pending job's limit can be changed.
func pendingCluster(name string, args []string) fake.Response {
	resp := liveCluster(name, args)
	resp.Stdout = jatStartTime.ReplaceAll(resp.Stdout, nil)
	return resp
}

var jatStartTime = regexp.MustCompile(`<JAT_start_time>[^<]*</JAT_start_time>`)

// qalterCall returns the recorded qalter argv.
func qalterCall(r *fake.Runner) []string {
	for _, c := range r.Calls {
		if c.Name == "qalter" {
			return c.Args
		}
	}
	return nil
}

var _ = Describe("scontrol update job", func() {
	DescribeTable("merges the new h_rt into the job's current hard resource list",
		func(args []string, want []string) {
			r := &fake.Runner{Responder: pendingCluster}
			var errBuf bytes.Buffer
			Expect(run(r, nil, append([]string{"update"}, args...), io.Discard, &errBuf)).To(Equal(0), errBuf.String())
			Expect(qalterCall(r)).To(Equal(want))
		},
		// 1399 requested gpu=1,mem_free=1G and no h_rt: both survive.
		Entry("minutes", []string{"JobId=1399", "TimeLimit=90"},
			[]string{"-l", "gpu=1,mem_free=1G,h_rt=5400", "1399"}),
		// 1400 is sbatch --signal=USR1@60: s_rt keeps its 60s distance.
		Entry("HH:MM:SS keeps the --signal gap", []string{"jobid=1400", "timelimit=1:00:00"},
			[]string{"-l", "h_rt=3600,mem_free=1G,s_rt=3540", "1400"}),
		Entry("D-HH", []string{"job=1400", "TimeLimit=1-0"},
			[]string{"-l", "h_rt=86400,mem_free=1G,s_rt=86340", "1400"}),
		Entry("UNLIMITED drops h_rt and the warning s_rt", []string{"JobId=1400", "TimeLimit=UNLIMITED"},
			[]string{"-l", "mem_free=1G", "1400"}),
		Entry("UNLIMITED on a job with nothing else clears the list", []string{"JobId=1376", "TimeLimit=UNLIMITED"},
			[]string{"-l", "", "1376"}),
		Entry("name and time in one call", []string{"JobId=1399", "JobName=renamed", "TimeLimit=10:00"},
			[]string{"-N", "renamed", "-l", "gpu=1,mem_free=1G,h_rt=600", "1399"}),
	)

	It("renames without reading the job", func() {
		r := &fake.Runner{}
		Expect(run(r, nil, []string{"update", "JobId=4711", "JobName=x"}, io.Discard, io.Discard)).To(Equal(0))
		Expect(r.Calls).To(HaveLen(1))
		Expect(r.Calls[0].Args).To(Equal([]string{"-N", "x", "4711"}))
	})

	DescribeTable("refuses before touching GE",
		func(args []string, want string) {
			r := &fake.Runner{Responder: liveCluster}
			var errBuf bytes.Buffer
			Expect(run(r, nil, append([]string{"update"}, args...), io.Discard, &errBuf)).To(Equal(1))
			Expect(errBuf.String()).To(HavePrefix("scontrol: error: "))
			Expect(errBuf.String()).To(ContainSubstring(want))
			Expect(qalterCall(r)).To(BeNil())
		},
		Entry("Partition", []string{"JobId=1399", "Partition=gpu"}, "update of Partition is not supported"),
		Entry("NumNodes", []string{"JobId=1399", "NumNodes=2"}, "update of NumNodes is not supported"),
		Entry("any other field", []string{"JobId=1399", "Priority=5"}, "update of Priority is not supported"),
		Entry("a bad field after a good one", []string{"JobId=1399", "TimeLimit=10", "Nice=5"}, "Nice"),
		Entry("relative time", []string{"JobId=1399", "TimeLimit=+10"}, "relative limits"),
		Entry("malformed time", []string{"JobId=1399", "TimeLimit=1:2:3:4"}, "invalid TimeLimit"),
		Entry("zero time", []string{"JobId=1399", "TimeLimit=0"}, "invalid TimeLimit"),
		Entry("no job", []string{"TimeLimit=10"}, "no job specified"),
		Entry("array task", []string{"JobId=1377_1", "TimeLimit=10"}, "single array task"),
		Entry("unknown job", []string{"JobId=999999", "TimeLimit=10"}, "Invalid job id specified"),
		Entry("not Key=Value", []string{"JobId=1399", "TimeLimit"}, "expected Key=Value"),
		Entry("nothing to change", []string{"JobId=1399"}, "nothing to update"),
		// 1399 and 1400 are running: execd keeps the limit it started them with.
		Entry("time of a running job", []string{"JobId=1399", "TimeLimit=90"}, "job 1399 has started"),
		Entry("time of a running job, with a rename", []string{"JobId=1400", "JobName=x", "TimeLimit=90"},
			"only be changed while it is pending"),
	)

	It("passes qalter's refusal through", func() {
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			if name == "qalter" {
				return fake.Response{Exit: 1, Stderr: []byte("denied: job \"4711\" does not exist")}
			}
			return fake.Response{}
		}}
		var errBuf bytes.Buffer
		Expect(run(r, nil, []string{"update", "JobId=4711", "JobName=x"}, io.Discard, &errBuf)).To(Equal(1))
		Expect(errBuf.String()).To(Equal("scontrol: error: denied: job \"4711\" does not exist\n"))
	})

	It("keeps every other request in place", func() {
		res := []gedata.Resource{{Name: "gpu", Value: "1"}, {Name: "h_rt", Value: "600"}, {Name: "lic", Value: "2"}}
		Expect(mergeTimeLimit(res, 1200, false)).To(Equal([]gedata.Resource{
			{Name: "gpu", Value: "1"}, {Name: "h_rt", Value: "1200"}, {Name: "lic", Value: "2"},
		}))
		// A user's own s_rt above h_rt is not a --signal gap; it stays as set.
		res = []gedata.Resource{{Name: "h_rt", Value: "600"}, {Name: "s_rt", Value: "900"}}
		Expect(gedata.ResourceList(mergeTimeLimit(res, 1200, false))).To(Equal("h_rt=1200,s_rt=900"))
		// A gap that no longer fits under the new limit is dropped.
		res = []gedata.Resource{{Name: "h_rt", Value: "3600"}, {Name: "s_rt", Value: "1800"}}
		Expect(gedata.ResourceList(mergeTimeLimit(res, 1000, false))).To(Equal("h_rt=1000"))
	})
})

var _ = Describe("scontrol dry run: job changes [SLURM_SHIM_DRY_RUN]", func() {
	BeforeEach(func() { GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1") })

	DescribeTable("reports the GE call and changes nothing",
		func(args []string, want string) {
			inner := &fake.Runner{Responder: pendingCluster}
			var errBuf bytes.Buffer
			runner := dryrun.Wrap(inner, &errBuf, "scontrol")
			Expect(run(runner, nil, args, io.Discard, &errBuf)).To(Equal(0))
			Expect(errBuf.String()).To(ContainSubstring("would run: " + want))
			for _, c := range inner.Calls {
				Expect(c.Name).To(Equal("qstat"), "only read-only clients reach the cluster")
			}
		},
		Entry("hold", []string{"hold", "4711_2"}, "qhold 4711.3"),
		Entry("release", []string{"release", "4711"}, "qrls 4711"),
		Entry("suspend", []string{"suspend", "4711"}, "qmod -sj 4711"),
		Entry("resume", []string{"resume", "4711"}, "qmod -usj 4711"),
		Entry("update", []string{"update", "JobId=1399", "TimeLimit=30"}, "qalter -l gpu=1,mem_free=1G,h_rt=1800 1399"),
	)
})
