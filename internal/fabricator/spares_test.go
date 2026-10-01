package fabricator_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/fabricator"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

var _ = Describe("hot spares [sbatch --x-spares]", func() {
	hostfile := "node001 2 all.q@node001 UNDEFINED\n" +
		"node002 2 all.q@node002 UNDEFINED\n" +
		"node003 2 all.q@node003 UNDEFINED\n"

	// One RSMAP GPU grant per host, so a spare's own grant is visible.
	gpuRunner := func() *fake.Runner {
		grant := func(host, id string) string {
			return "<element><GRU_name>gpu</GRU_name><GRU_host>" + host + "</GRU_host>" +
				"<GRU_resource_map_list><element><RESL_value>" + id + "</RESL_value></element></GRU_resource_map_list></element>"
		}
		xml := "<detailed_job_info><djob_info><element><JB_ja_tasks><element><JAT_granted_resources_list>" +
			grant("node001", "0") + grant("node002", "1") + grant("node003", "2") +
			"</JAT_granted_resources_list></element></JB_ja_tasks></element></djob_info></detailed_job_info>"
		return &fake.Runner{Responder: func(string, []string) fake.Response { return fake.Response{Stdout: []byte(xml)} }}
	}
	has := func(res *fabricator.Result, key string) bool {
		for _, kv := range res.Exports {
			if kv.Key == key {
				return true
			}
		}
		return false
	}
	export := func(res *fabricator.Result, key string) string {
		for _, kv := range res.Exports {
			if kv.Key == key {
				return kv.Value
			}
		}
		return ""
	}

	It("keeps the last k granted hosts out of the job and the SLURM_* node variables", func() {
		res := hostfileFab(hostfile, map[string]string{"SLURM_SHIM_SPARES": "1"}, gpuRunner(), config.Default())

		Expect(res.Layout.Nodes).To(HaveLen(2))
		Expect(res.Layout.Nodes[0].Host).To(Equal("node001"), "the master is never a spare")
		Expect(res.Layout.Nodes[1].Host).To(Equal("node002"))
		Expect(export(res, "SLURM_NNODES")).To(Equal("2"))
		Expect(export(res, "SLURM_JOB_NODELIST")).To(Equal("node[001-002]"))
		Expect(export(res, "SLURM_X_SPARE_NODELIST")).To(Equal("node003"))
		Expect(res.Layout.Rendezvous.MasterAddr).To(Equal("node001"))
	})

	It("keeps each spare's own GPU grant in the layout, ready for the node it replaces", func() {
		res := hostfileFab(hostfile, map[string]string{"SLURM_SHIM_SPARES": "1"}, gpuRunner(), config.Default())

		Expect(res.Layout.Spares).To(HaveLen(1))
		Expect(res.Layout.Spares[0].Host).To(Equal("node003"))
		Expect(res.Layout.Spares[0].GPUs).To(Equal([]string{"2"}))
		Expect(res.Layout.Spares[0].IsMaster).To(BeFalse())
	})

	It("runs without spares, with a warning, when they would leave only the master", func() {
		res := hostfileFab(hostfile, map[string]string{"SLURM_SHIM_SPARES": "3"}, gpuRunner(), config.Default())

		Expect(res.Layout.Nodes).To(HaveLen(3))
		Expect(res.Layout.Spares).To(BeEmpty())
		Expect(res.Warnings).To(ContainElement(ContainSubstring("running without spares")))
		Expect(has(res, "SLURM_X_SPARE_NODELIST")).To(BeFalse())
	})

	It("changes nothing for a job without spares", func() {
		r := gpuRunner()
		res := hostfileFab(hostfile, nil, r, config.Default())

		Expect(res.Layout.Nodes).To(HaveLen(3))
		Expect(res.Layout.Spares).To(BeNil())
		Expect(has(res, "SLURM_X_SPARE_NODELIST")).To(BeFalse())
		for _, c := range r.Calls {
			Expect(c.Name).NotTo(Equal("qalter"), "no job-context write for a job without spares")
		}
	})

	// sbatch overrides an inherited count with -v SLURM_SHIM_SPARES=0 (todo 138), so
	// a chained job from inside a spares job must keep every node.
	It("treats SLURM_SHIM_SPARES=0 or empty as a job without spares", func() {
		for _, v := range []string{"0", ""} {
			r := gpuRunner()
			res := hostfileFab(hostfile, map[string]string{"SLURM_SHIM_SPARES": v}, r, config.Default())

			Expect(res.Layout.Nodes).To(HaveLen(3), "SLURM_SHIM_SPARES=%q", v)
			Expect(res.Layout.Spares).To(BeNil())
			Expect(export(res, "SLURM_NNODES")).To(Equal("3"))
			Expect(has(res, "SLURM_X_SPARE_NODELIST")).To(BeFalse())
		}
	})

	It("predicts the same split for a dry run", func() {
		nodes := []fabricator.PredictedNode{{Name: "node01", Slots: 2}, {Name: "node02", Slots: 2}, {Name: "node03", Slots: 2}}
		res, err := predict(map[string]string{"SLURM_SHIM_SPARES": "1"}, nodes, config.Default())

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Layout.Nodes).To(HaveLen(2))
		Expect(res.Layout.Spares).To(HaveLen(1))
		Expect(export(res, "SLURM_NNODES")).To(Equal("2"))
		Expect(export(res, "SLURM_X_SPARE_NODELIST")).To(Equal("node03"))
	})

	// [AC11] A requeued job is granted afresh: the previous run's swaps must not
	// show in squeue or scontrol, so each fabrication clears them.
	It("clears the previous run's recorded swaps", func() {
		r := gpuRunner()
		hostfileFab(hostfile, map[string]string{"SLURM_SHIM_SPARES": "1"}, r, config.Default())

		var qalter []string
		for _, c := range r.Calls {
			if c.Name == "qalter" {
				qalter = c.Args
			}
		}
		Expect(qalter).To(ContainElements("-dc", "shim.swaps"))
	})
})
