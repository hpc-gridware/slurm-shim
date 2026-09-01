package fabricator_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/fabricator"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// predict runs Predict with the same fixture Identity the fab helper uses, so a
// parity assertion compares environments and not identities.
func predict(env map[string]string, nodes []fabricator.PredictedNode, cfg *config.Config) (*fabricator.Result, error) {
	m := map[string]string{}
	for k, v := range env {
		m[k] = v
	}
	return fabricator.Predict(fabricator.Options{
		Env:    func(k string) string { return m[k] },
		Config: cfg,
		Identity: fake.Identity{
			HostnameVal: "node001",
			UserName:    "alice",
			UID:         1000,
			GID:         1000,
		},
		NowUnix: 1754481600,
	}, nodes)
}

// grantDependent are the exports only a real allocation can supply. Everything
// else must match between a prediction and the fabrication of the same shape --
// that equivalence is the whole reason assemble() was extracted.
var grantDependent = map[string]bool{
	"SLURM_JOB_ID": true, "SLURM_JOBID": true, "SLURM_ARRAY_JOB_ID": true,
	"SLURM_JOB_NODELIST": true, "SLURM_NODELIST": true,
	"SLURM_LAUNCH_NODE_IPADDR": true, "SLURM_JOB_GPUS": true,
	"SLURM_MEM_PER_NODE": true, "MASTER_ADDR": true, "MASTER_PORT": true,
}

func withoutGrant(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if !grantDependent[k] {
			out[k] = v
		}
	}
	return out
}

var _ = Describe("Predict [REQ-DRY-005]", func() {
	env := func(pe string) map[string]string {
		return map[string]string{
			"JOB_ID": "4711", "JOB_NAME": "train", "PE": pe,
			"QUEUE": "all.q", "SGE_TASK_ID": "undefined",
		}
	}
	homogeneous := "node001 8 all.q@node001 0-7\nnode002 8 all.q@node002 0-7\n"
	twoByEight := []fabricator.PredictedNode{
		{Name: "node001", Slots: 8},
		{Name: "node002", Slots: 8},
	}

	// The contract the Fabricate/assemble split exists to guarantee. Without this
	// spec, a change to the fabricator's inputs can make the dry run lie while the
	// whole suite stays green.
	DescribeTable("predicts the same environment the fabricator produces",
		func(pe string) {
			real, err := fab(env(pe), homogeneous, testConfig())
			Expect(err).NotTo(HaveOccurred())

			predicted, err := predict(env(pe), twoByEight, testConfig())
			Expect(err).NotTo(HaveOccurred())

			Expect(withoutGrant(exportMap(predicted))).
				To(Equal(withoutGrant(exportMap(real))))
		},
		Entry("slot policy", "mpi.pe"),
		Entry("node policy", "smp.pe"),
	)

	It("honors the SLURM_SHIM_TASK_POLICY override exactly as Fabricate does", func() {
		e := env("smp.pe")
		e["SLURM_SHIM_TASK_POLICY"] = "slot"

		real, err := fab(e, homogeneous, testConfig())
		Expect(err).NotTo(HaveOccurred())
		predicted, err := predict(e, twoByEight, testConfig())
		Expect(err).NotTo(HaveOccurred())

		Expect(exportMap(predicted)["SLURM_NTASKS"]).To(Equal(exportMap(real)["SLURM_NTASKS"]))
		Expect(exportMap(predicted)["SLURM_NTASKS"]).To(Equal("16"))
	})

	It("reports scrub-only mode under SLURM_SHIM_DISABLE, as the real job gets", func() {
		e := env("mpi.pe")
		e["SLURM_SHIM_DISABLE"] = "1"

		res, err := predict(e, twoByEight, testConfig())
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Disabled).To(BeTrue())
		Expect(res.Exports).To(BeEmpty())
		Expect(res.Unset).NotTo(BeEmpty())
	})

	It("derives the gpu task policy from the modelled device counts", func() {
		cfg := testConfig()
		cfg.PEs["gpu.pe"] = config.PE{TaskPolicy: "gpu"}
		res, err := predict(env("gpu.pe"), []fabricator.PredictedNode{
			{Name: "node001", Slots: 8, GPUs: 2},
			{Name: "node002", Slots: 8, GPUs: 2},
		}, cfg)

		Expect(err).NotTo(HaveOccurred())
		m := exportMap(res)
		Expect(m["SLURM_NTASKS"]).To(Equal("4"))
		Expect(m["SLURM_GPUS_ON_NODE"]).To(Equal("2"))
		Expect(m["SLURM_CPUS_PER_TASK"]).To(Equal("4"))
	})

	It("omits the memory grant rather than guessing it", func() {
		res, err := predict(env("mpi.pe"), twoByEight, testConfig())
		Expect(err).NotTo(HaveOccurred())
		_, present := exportMap(res)["SLURM_MEM_PER_NODE"]
		Expect(present).To(BeFalse())
	})

	It("rejects an empty allocation instead of panicking", func() {
		_, err := predict(env("mpi.pe"), nil, testConfig())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("at least one node"))
	})

	It("falls back to the default config when none is given", func() {
		res, err := predict(env("mpi.pe"), twoByEight, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Exports).NotTo(BeEmpty())
	})

	It("scrubs SLURM_SHIM_DRY_RUN so it cannot survive into a job", func() {
		res, err := predict(env("mpi.pe"), twoByEight, testConfig())
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Unset).To(ContainElement("SLURM_SHIM_DRY_RUN"))
	})
})
