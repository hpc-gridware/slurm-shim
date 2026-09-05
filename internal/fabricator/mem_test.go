package fabricator_test

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

var _ = Describe("SLURM_MEM_PER_NODE discovery [A27]", func() {
	// memRunner replays the captured h_vmem=2G request for `qstat -xml -j`.
	memRunner := func() *fake.Runner {
		xml, err := os.ReadFile("../gedata/testdata/qstat_j_mem.xml")
		Expect(err).NotTo(HaveOccurred())
		return &fake.Runner{Responder: func(name string, args []string) fake.Response {
			return fake.Response{Stdout: xml}
		}}
	}

	It("emits per-slot h_vmem x slots floored to MB", func() {
		// 2 GiB per slot = 2048 MB; 4 slots -> 8192 MB. The complex is pinned to the
		// one the fixture records; the shipped default is mem_free.
		cfg := testConfig()
		cfg.MemoryComplex = "h_vmem"
		r := hostfileFab("node001 4 all.q@node001 0-3\n", map[string]string{"JOB_ID": "268", "PE": "smp.pe"},
			memRunner(), cfg)
		Expect(exportMap(r)["SLURM_MEM_PER_NODE"]).To(Equal("8192"))
	})

	It("scales with the master node's slot count", func() {
		cfg := testConfig()
		cfg.MemoryComplex = "h_vmem"
		r := hostfileFab("node001 1 all.q@node001 0\n", map[string]string{"JOB_ID": "268", "PE": "smp.pe"},
			memRunner(), cfg)
		Expect(exportMap(r)["SLURM_MEM_PER_NODE"]).To(Equal("2048"))
	})

	It("omits SLURM_MEM_PER_NODE when memory_complex is disabled", func() {
		cfg := testConfig()
		cfg.MemoryComplex = ""
		r := hostfileFab("node001 4 all.q@node001 0-3\n", map[string]string{"JOB_ID": "268", "PE": "smp.pe"},
			memRunner(), cfg)
		_, present := exportMap(r)["SLURM_MEM_PER_NODE"]
		Expect(present).To(BeFalse())
	})

	It("omits SLURM_MEM_PER_NODE when the job requested no memory", func() {
		// qstat_j_gpu1.xml has no h_vmem request.
		gpuXML, err := os.ReadFile("../gedata/testdata/qstat_j_gpu1.xml")
		Expect(err).NotTo(HaveOccurred())
		runner := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: gpuXML}
		}}
		r := hostfileFab("node001 4 all.q@node001 0-3\n", map[string]string{"JOB_ID": "266", "PE": "smp.pe"},
			runner, testConfig())
		_, present := exportMap(r)["SLURM_MEM_PER_NODE"]
		Expect(present).To(BeFalse())
	})

	It("warns and omits SLURM_MEM_PER_NODE when qstat fails", func() {
		runner := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qstat: cannot connect")}
		}}
		r := hostfileFab("node001 4 all.q@node001 0-3\n", map[string]string{"JOB_ID": "268", "PE": "smp.pe"},
			runner, testConfig())
		_, present := exportMap(r)["SLURM_MEM_PER_NODE"]
		Expect(present).To(BeFalse())
		Expect(r.Warnings).To(ContainElement(ContainSubstring("memory discovery failed")))
	})

	It("scrubs a pre-existing SLURM_MEM_PER_NODE when discovery yields nothing", func() {
		cfg := testConfig()
		cfg.MemoryComplex = ""
		r := hostfileFab("node001 4 all.q@node001 0-3\n", map[string]string{"JOB_ID": "268", "PE": "smp.pe"},
			memRunner(), cfg)
		Expect(r.Unset).To(ContainElement("SLURM_MEM_PER_NODE"))
	})
})

var _ = Describe("config default for memory discovery", func() {
	It("defaults memory_complex to mem_free (h_vmem caps RLIMIT_AS and kills CUDA)", func() {
		Expect(config.Default().MemoryComplex).To(Equal("mem_free"))
	})
})
