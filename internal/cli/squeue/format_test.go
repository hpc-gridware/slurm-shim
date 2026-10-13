package squeue

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

var _ = Describe("squeue field rendering [REQ-SQU-002]", func() {
	DescribeTable("sizes a value to its field as squeue does",
		func(value string, width int, right bool, want string) {
			Expect(fit(value, width, right)).To(Equal(want))
		},
		Entry("unsized", "sqI_arrsplit", 0, false, "sqI_arrsplit"),
		Entry("right-justified", "PD", 4, true, "  PD"),
		Entry("left-justified", "PD", 4, false, "PD  "),
		Entry("cut to the field", "sqI_arrsplit", 8, true, "sqI_arrs"),
	)

	DescribeTable("renders MIN_MEMORY in the largest exact unit",
		func(bytes float64, want string) {
			Expect(slurmMemory(bytes)).To(Equal(want))
		},
		Entry("megabytes", 500.0*(1<<20), "500M"),
		Entry("gigabytes", 2.0*(1<<30), "2G"),
		Entry("not a whole gigabyte", 1.5*(1<<30), "1536M"),
		Entry("terabytes", 1.0*(1<<40), "1T"),
	)

	It("renders a GPU request as SLURM's TRES per node", func() {
		v := view{cfg: config.Default(), details: map[string]gedata.JobInfo{
			"7": {Requests: map[string]gedata.ResourceRequest{"gpu": {Value: "2", Amount: 2}}},
		}}
		Expect(v.rowValue('b', gedata.JobRow{JobID: "7", State: "qw"})).To(Equal("gres/gpu:2"))
		Expect(v.rowValue('b', gedata.JobRow{JobID: "8", State: "qw"})).To(Equal("N/A"))
	})

	DescribeTable("names a job's partition",
		func(row gedata.JobRow, detail gedata.JobInfo, want string) {
			cfg := config.Default()
			cfg.Partitions = map[string]config.Partition{
				"mpi":    {Queue: "all.q", PE: "smp"},
				"serial": {Queue: "all.q"},
				"gpu":    {Queue: "gpu.q", PE: "smp"},
				"batch":  {Queue: "all.q", PE: "make"},
			}
			cfg.DefaultPartition = "batch"
			cfg.PartitionAliases = map[string]string{"old.q": "legacy"}
			v := view{cfg: cfg, details: map[string]gedata.JobInfo{"1": detail}}
			row.JobID = "1"
			Expect(v.partition(row)).To(Equal(want))
		},
		Entry("by queue and PE", gedata.JobRow{Queue: "all.q@n1"}, gedata.JobInfo{PE: "smp"}, "mpi"),
		Entry("by queue, no PE", gedata.JobRow{Queue: "all.q@n1"}, gedata.JobInfo{}, "serial"),
		Entry("by queue, preferring the default partition", gedata.JobRow{Queue: "all.q@n1"}, gedata.JobInfo{PE: "mpi"}, "batch"),
		Entry("by the queue a pending job requested", gedata.JobRow{}, gedata.JobInfo{Queues: []string{"gpu.q"}}, "gpu"),
		Entry("by an alias", gedata.JobRow{Queue: "old.q@n1"}, gedata.JobInfo{}, "legacy"),
		Entry("by the queue's own name", gedata.JobRow{Queue: "other.q@n1"}, gedata.JobInfo{}, "other.q"),
		Entry("not at all for a pending job that named nothing", gedata.JobRow{}, gedata.JobInfo{}, ""),
	)

	It("reports Resources for a job the scheduler left a message for, Priority otherwise", func() {
		v := view{cfg: config.Default(), schedInfo: true, now: time.Now(), details: map[string]gedata.JobInfo{
			"7": {SchedulerMessages: []string{"cannot run in queue all.q"}},
			"8": {},
		}}
		Expect(v.reason(gedata.JobRow{JobID: "7", State: "qw"})).To(Equal("Resources"))
		Expect(v.reason(gedata.JobRow{JobID: "8", State: "qw"})).To(Equal("Priority"))
	})

	It("parses an -O field's size, justification and suffix", func() {
		toks, unknown := tokenizeLong("JobID,name:.12|, nosuch ,State:5x")
		Expect(unknown).To(Equal([]string{"nosuch"}))
		Expect(toks).To(Equal([]token{
			{isField: true, verb: 'i', width: 20},
			{isField: true, verb: 'j', width: 12, right: true, suffix: "|"},
			{isField: true, verb: 'T', width: 5, suffix: "x"},
		}))
	})
})
