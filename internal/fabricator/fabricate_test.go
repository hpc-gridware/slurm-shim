package fabricator_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Table A fabrication [REQ-TST-004]", func() {
	baseEnv := func(pe string) map[string]string {
		return map[string]string{
			"JOB_ID": "4711", "JOB_NAME": "train", "PE": pe,
			"QUEUE": "all.q", "SGE_TASK_ID": "undefined",
		}
	}
	homogeneous := "node001 8 all.q@node001 0-7\nnode002 8 all.q@node002 0-7\n"

	Describe("slot policy over a homogeneous 2x8 allocation [REQ-ENV-001]", func() {
		It("emits the core job-level contract [REQ-ENV-020]", func() {
			r, err := fab(baseEnv("mpi.pe"), homogeneous, testConfig())
			Expect(err).NotTo(HaveOccurred())
			m := exportMap(r)
			Expect(m["SLURM_JOB_ID"]).To(Equal("4711"))
			Expect(m["SLURM_JOBID"]).To(Equal("4711"))
			Expect(m["SLURM_NNODES"]).To(Equal("2"))
			Expect(m["SLURM_JOB_NUM_NODES"]).To(Equal("2"))
			Expect(m["SLURM_JOB_NODELIST"]).To(Equal("node[001-002]"))
			Expect(m["SLURM_NODELIST"]).To(Equal("node[001-002]"))
			Expect(m["SLURM_NTASKS"]).To(Equal("16"))
			Expect(m["SLURM_NPROCS"]).To(Equal("16"))
			Expect(m["SLURM_TASKS_PER_NODE"]).To(Equal("8(x2)"))
			Expect(m["SLURM_NTASKS_PER_NODE"]).To(Equal("8"))
			Expect(m["SLURM_DISTRIBUTION"]).To(Equal("block"))
			Expect(m["SLURM_PROCID"]).To(Equal("0"))
			Expect(m["SLURM_JOB_PARTITION"]).To(Equal("batch"))
			Expect(m["SLURM_JOB_USER"]).To(Equal("alice"))
			Expect(m["SLURM_LAUNCH_NODE_IPADDR"]).To(Equal("10.0.0.11"))
		})

		It("omits cpus_per_task for the slot policy [REQ-ENV-001]", func() {
			r, _ := fab(baseEnv("mpi.pe"), homogeneous, testConfig())
			_, present := exportMap(r)["SLURM_CPUS_PER_TASK"]
			Expect(present).To(BeFalse())
		})
	})

	Describe("node policy [REQ-ENV-001]", func() {
		It("emits one task per node with per-node slots as cpus_per_task", func() {
			r, err := fab(baseEnv("smp.pe"), homogeneous, testConfig())
			Expect(err).NotTo(HaveOccurred())
			m := exportMap(r)
			Expect(m["SLURM_NTASKS"]).To(Equal("2"))
			Expect(m["SLURM_TASKS_PER_NODE"]).To(Equal("1(x2)"))
			Expect(m["SLURM_CPUS_PER_TASK"]).To(Equal("8"))
			Expect(m["SLURM_CPUS_ON_NODE"]).To(Equal("8"))
			Expect(m["SLURM_JOB_CPUS_PER_NODE"]).To(Equal("8(x2)"))
		})
	})

	Describe("heterogeneous slots under the slot policy [REQ-ENV-001]", func() {
		It("omits SLURM_NTASKS_PER_NODE and warns [REQ-ENV-041]", func() {
			r, err := fab(baseEnv("mpi.pe"), "node001 8 all.q@node001\nnode002 4 all.q@node002\n", testConfig())
			Expect(err).NotTo(HaveOccurred())
			m := exportMap(r)
			Expect(m["SLURM_TASKS_PER_NODE"]).To(Equal("8,4"))
			Expect(m["SLURM_NTASKS"]).To(Equal("12"))
			_, present := m["SLURM_NTASKS_PER_NODE"]
			Expect(present).To(BeFalse())
			Expect(r.Warnings).To(ContainElement(ContainSubstring("Lightning requires it")))
		})
	})

	Describe("duplicate host lines [REQ-LAY-003]", func() {
		It("merges slots and yields one node", func() {
			r, err := fab(baseEnv("mpi.pe"),
				"node001 4 all.q@node001\nnode001 4 gpu.q@node001\n", testConfig())
			Expect(err).NotTo(HaveOccurred())
			m := exportMap(r)
			Expect(m["SLURM_NNODES"]).To(Equal("1"))
			Expect(m["SLURM_NTASKS"]).To(Equal("8"))
			Expect(r.Layout.Nodes[0].Slots).To(Equal(8))
		})
	})

	Describe("single-node fallback for a non-PE job [REQ-FAB-011]", func() {
		It("fabricates a localhost layout when PE_HOSTFILE is absent and NHOSTS<=1", func() {
			env := map[string]string{"JOB_ID": "42", "JOB_NAME": "batch", "NSLOTS": "4"}
			r, err := fab(env, "", testConfig())
			Expect(err).NotTo(HaveOccurred())
			m := exportMap(r)
			Expect(m["SLURM_NNODES"]).To(Equal("1"))
			Expect(r.Layout.Nodes[0].Host).To(Equal("node001"))
			Expect(r.Layout.Nodes[0].Slots).To(Equal(4))
			Expect(m["SLURM_NTASKS"]).To(Equal("1")) // node policy default
		})
	})

	Describe("array-job guards [REQ-ENV-010]", func() {
		It("emits array vars only for a numeric SGE_TASK_ID", func() {
			env := baseEnv("mpi.pe")
			env["SGE_TASK_ID"] = "2"
			env["SGE_TASK_FIRST"] = "1"
			env["SGE_TASK_LAST"] = "4"
			env["SGE_TASK_STEPSIZE"] = "1"
			r, _ := fab(env, homogeneous, testConfig())
			m := exportMap(r)
			Expect(m["SLURM_ARRAY_JOB_ID"]).To(Equal("4711"))
			Expect(m["SLURM_ARRAY_TASK_ID"]).To(Equal("2"))
			Expect(m["SLURM_ARRAY_TASK_COUNT"]).To(Equal("4"))
		})

		It("maps GE 1-based tasks to a 0-based SLURM index via SLURM_ARRAY_BASE [submitit Phase 3]", func() {
			// sbatch --array=0-9 submits GE `-t 1-10` + SLURM_ARRAY_BASE=0.
			env := baseEnv("mpi.pe")
			env["SGE_TASK_ID"] = "1" // first GE task
			env["SGE_TASK_FIRST"] = "1"
			env["SGE_TASK_LAST"] = "10"
			env["SGE_TASK_STEPSIZE"] = "1"
			env["SLURM_ARRAY_BASE"] = "0"
			env["SLURM_ARRAY_STEP"] = "1"
			r, _ := fab(env, homogeneous, testConfig())
			m := exportMap(r)
			Expect(m["SLURM_ARRAY_TASK_ID"]).To(Equal("0"))
			Expect(m["SLURM_ARRAY_TASK_MIN"]).To(Equal("0"))
			Expect(m["SLURM_ARRAY_TASK_MAX"]).To(Equal("9"))
			Expect(m["SLURM_ARRAY_TASK_STEP"]).To(Equal("1"))
			Expect(m["SLURM_ARRAY_TASK_COUNT"]).To(Equal("10"))
		})

		It("offsets a mid-array GE task to its 0-based SLURM index", func() {
			env := baseEnv("mpi.pe")
			env["SGE_TASK_ID"] = "10" // last GE task
			env["SGE_TASK_FIRST"] = "1"
			env["SGE_TASK_LAST"] = "10"
			env["SGE_TASK_STEPSIZE"] = "1"
			env["SLURM_ARRAY_BASE"] = "0"
			env["SLURM_ARRAY_STEP"] = "1"
			r, _ := fab(env, homogeneous, testConfig())
			m := exportMap(r)
			Expect(m["SLURM_ARRAY_TASK_ID"]).To(Equal("9"))
		})

		It("reconstructs a stepped SLURM array from a dense GE range", func() {
			// sbatch --array=0-10:2 submits GE `-t 1-6` + BASE=0 STEP=2.
			env := baseEnv("mpi.pe")
			env["SGE_TASK_ID"] = "3" // 3rd dense GE task
			env["SGE_TASK_FIRST"] = "1"
			env["SGE_TASK_LAST"] = "6"
			env["SGE_TASK_STEPSIZE"] = "1"
			env["SLURM_ARRAY_BASE"] = "0"
			env["SLURM_ARRAY_STEP"] = "2"
			r, _ := fab(env, homogeneous, testConfig())
			m := exportMap(r)
			Expect(m["SLURM_ARRAY_TASK_ID"]).To(Equal("4")) // 0,2,4 -> 3rd is 4
			Expect(m["SLURM_ARRAY_TASK_MAX"]).To(Equal("10"))
			Expect(m["SLURM_ARRAY_TASK_COUNT"]).To(Equal("6"))
		})

		It("emits no array vars for the literal undefined [REQ-ENV-010]", func() {
			r, _ := fab(baseEnv("mpi.pe"), homogeneous, testConfig())
			m := exportMap(r)
			_, present := m["SLURM_ARRAY_TASK_ID"]
			Expect(present).To(BeFalse())
		})

		It("folds the task id into the rendezvous port [REQ-ENV-001]", func() {
			env := baseEnv("mpi.pe")
			env["SGE_TASK_ID"] = "3"
			r, _ := fab(env, homogeneous, testConfig())
			// base 20000 + ((4711*31 + 3) mod 10000)
			Expect(r.Layout.Rendezvous.MasterPort).To(Equal(20000 + int((4711*31+3)%10000)))
		})
	})

	Describe("disable and scrub [REQ-ENV-011]", func() {
		It("produces only the unset preamble when disabled", func() {
			env := baseEnv("mpi.pe")
			env["SLURM_SHIM_DISABLE"] = "1"
			r, err := fab(env, homogeneous, testConfig())
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Disabled).To(BeTrue())
			Expect(r.Exports).To(BeEmpty())
			Expect(r.Unset).To(ContainElement("SLURM_JOB_ID"))
			Expect(r.Unset).NotTo(ContainElement("SLURM_KILL_BAD_EXIT"))
		})
	})

	Describe("per-job task-policy override [REQ-FAB-012]", func() {
		It("honors SLURM_SHIM_TASK_POLICY over the PE default", func() {
			env := baseEnv("mpi.pe") // PE maps to slot
			env["SLURM_SHIM_TASK_POLICY"] = "node"
			r, _ := fab(env, homogeneous, testConfig())
			Expect(exportMap(r)["SLURM_NTASKS"]).To(Equal("2")) // node policy
		})
	})

	Describe("export invariants [REQ-FAB-006]", func() {
		It("passes the tasks-per-node sum and nodelist round-trip self-tests [REQ-FAB-007]", func() {
			// A successful fabrication means validate() enforced: sum of the N2
			// tasks-per-node equals ntasks, the N1 nodelist expands to nnodes,
			// and machine values pass the charset check.
			r, err := fab(baseEnv("mpi.pe"), homogeneous, testConfig())
			Expect(err).NotTo(HaveOccurred())
			Expect(exportMap(r)["SLURM_NTASKS"]).To(Equal("16"))
			Expect(exportMap(r)["SLURM_TASKS_PER_NODE"]).To(Equal("8(x2)"))
		})
	})

	Describe("policy errors surface as fabrication failures [REQ-ENC-005]", func() {
		It("fails when the gpu policy finds no GPUs", func() {
			_, err := fab(baseEnv("gpu.pe"), homogeneous, testConfig())
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("variables that must not be set [REQ-ENV-050]", func() {
		It("never emits forbidden SLURM_* or PMI variables", func() {
			r, _ := fab(baseEnv("mpi.pe"), homogeneous, testConfig())
			m := exportMap(r)
			for _, k := range []string{
				"SLURM_CONF", "SLURM_PRIO_PROCESS", "SLURM_UMASK",
				"SLURM_JOB_QOS", "SLURM_JOB_RESERVATION", "PMI_RANK", "PMIX_RANK",
			} {
				_, present := m[k]
				Expect(present).To(BeFalse(), k)
			}
		})
	})
})
