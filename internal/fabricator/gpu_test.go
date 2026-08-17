package fabricator_test

import (
	"os"
	"path/filepath"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/fabricator"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// hostfileFab runs the fabricator with a caller-supplied PE_HOSTFILE and Runner,
// so GPU discovery paths can be exercised end-to-end.
func hostfileFab(hostfile string, env map[string]string, runner *fake.Runner, cfg *config.Config) *fabricator.Result {
	path := filepath.Join(GinkgoT().TempDir(), "pe_hostfile")
	Expect(os.WriteFile(path, []byte(hostfile), 0o600)).To(Succeed())
	m := map[string]string{"JOB_ID": "267", "JOB_NAME": "train", "PE": "gpu.pe"}
	for k, v := range env {
		m[k] = v
	}
	m["PE_HOSTFILE"] = path
	opts := fabricator.Options{
		Env:      func(k string) string { return m[k] },
		Config:   cfg,
		Identity: fake.Identity{HostnameVal: "node001", UserName: "alice", UID: 1000, GID: 1000},
		NowUnix:  1754481600,
	}
	// Only set the interface field for a real Runner: assigning a typed nil
	// *fake.Runner would make Options.Runner a non-nil interface holding nil.
	if runner != nil {
		opts.Runner = runner
	}
	r, err := fabricator.Fabricate(opts)
	Expect(err).NotTo(HaveOccurred())
	return r
}

func xmlResponder(fixture string, expectArgs []string) *fake.Runner {
	xml, err := os.ReadFile("../gedata/testdata/" + fixture)
	Expect(err).NotTo(HaveOccurred())
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		Expect(name).To(Equal("qstat"))
		if expectArgs != nil {
			Expect(args).To(Equal(expectArgs))
		}
		return fake.Response{Stdout: xml}
	}}
}

var _ = Describe("GPU discovery end-to-end [REQ-GPU-001]", func() {
	It("populates node GPU env from the granted RSMAP (gpu=2)", func() {
		r := hostfileFab("ocs-worker2 2 all.q@ocs-worker2 0-1\n", nil,
			xmlResponder("qstat_j_gpu2.xml", []string{"-xml", "-j", "267"}), testConfig())

		Expect(r.Layout.Nodes[0].GPUs).To(Equal([]int{0, 1}))
		m := exportMap(r)
		Expect(m["SLURM_GPUS_ON_NODE"]).To(Equal("2"))
		Expect(m["SLURM_JOB_GPUS"]).To(Equal("0,1"))
	})

	It("matches a granted FQDN host to a short-named allocation node", func() {
		// The fixture grants on "ocs-worker2"; the hostfile lists its FQDN.
		r := hostfileFab("ocs-worker2.hpc.example 2 all.q@ocs-worker2.hpc.example 0-1\n", nil,
			xmlResponder("qstat_j_gpu2.xml", nil), testConfig())
		Expect(r.Layout.Nodes[0].GPUs).To(Equal([]int{0, 1}))
	})

	It("warns and leaves GPUs empty when the granted host is not in the allocation", func() {
		// Fixture grants on ocs-worker2, but the allocation node is otherhost.
		// Use a non-GPU policy so empty GPUs is not itself a fatal error.
		r := hostfileFab("otherhost 2 all.q@otherhost 0-1\n", map[string]string{"PE": "smp.pe"},
			xmlResponder("qstat_j_gpu2.xml", nil), testConfig())
		Expect(r.Layout.Nodes[0].GPUs).To(BeEmpty())
		Expect(r.Warnings).To(ContainElement(ContainSubstring("did not match any allocation node")))
	})

	It("falls back to the plain resource_map view when the XML view fails", func() {
		plain, err := os.ReadFile("../gedata/testdata/qstat_j_gpu2_plain.txt")
		Expect(err).NotTo(HaveOccurred())
		runner := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			if len(args) > 0 && args[0] == "-xml" {
				return fake.Response{Exit: 1, Stderr: []byte("xml view unavailable")}
			}
			return fake.Response{Stdout: plain} // qstat -j
		}}
		r := hostfileFab("ocs-worker2 2 all.q@ocs-worker2 0-1\n", nil, runner, testConfig())
		Expect(r.Layout.Nodes[0].GPUs).To(Equal([]int{0, 1}))
	})

	It("continues without GPU env when both qstat views fail", func() {
		runner := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qstat: cannot connect")}
		}}
		r := hostfileFab("node001 4 all.q@node001 0-3\n", map[string]string{"JOB_ID": "42", "PE": "smp.pe"},
			runner, testConfig())
		Expect(r.Layout.Nodes[0].GPUs).To(BeEmpty())
		Expect(r.Warnings).To(ContainElement(ContainSubstring("GPU discovery failed")))
	})

	It("uses SGE_HGR for a single node when no Runner is available", func() {
		r := hostfileFab("node001 2 all.q@node001 0-1\n",
			map[string]string{"JOB_ID": "42", "SGE_HGR_gpu": "0 1"}, nil, testConfig())
		Expect(r.Layout.Nodes[0].GPUs).To(Equal([]int{0, 1}))
	})

	It("does not use SGE_HGR for a multi-node job (not multi-host safe)", func() {
		r := hostfileFab("node001 2 all.q@node001 0-1\nnode002 2 all.q@node002 0-1\n",
			map[string]string{"JOB_ID": "42", "PE": "smp.pe", "SGE_HGR_gpu": "0 1"}, nil, testConfig())
		Expect(r.Layout.Nodes[0].GPUs).To(BeEmpty())
		Expect(r.Layout.Nodes[1].GPUs).To(BeEmpty())
	})

	It("does not emit a GPU-sharing warning for a plain non-GPU job", func() {
		r := hostfileFab("node001 4 all.q@node001 0-3\n",
			map[string]string{"JOB_ID": "42", "PE": "smp.pe"}, nil, testConfig())
		for _, w := range r.Warnings {
			Expect(w).NotTo(ContainSubstring("share"))
		}
	})
})

var _ = Describe("nvidia-smi discovery [REQ-GPU-001]", func() {
	nvidiaCfg := func() *config.Config {
		c := testConfig()
		c.GPU.Discovery = "nvidia-smi"
		c.MemoryComplex = "" // isolate: these specs exercise GPU discovery only
		return c
	}

	It("reads local indices and warns they are physical, not granted", func() {
		runner := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			Expect(name).To(Equal("nvidia-smi"))
			return fake.Response{Stdout: []byte("0\n1\n")}
		}}
		r := hostfileFab("node001 2 all.q@node001 0-1\n", map[string]string{"PE": "smp.pe"}, runner, nvidiaCfg())
		Expect(r.Layout.Nodes[0].GPUs).To(Equal([]int{0, 1}))
		Expect(r.Warnings).To(ContainElement(ContainSubstring("physical")))
	})

	It("warns and skips remote hosts on a multi-node job", func() {
		runner := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte("0\n1\n")}
		}}
		r := hostfileFab("node001 2 all.q@node001 0-1\nnode002 2 all.q@node002 0-1\n",
			map[string]string{"PE": "smp.pe"}, runner, nvidiaCfg())
		Expect(r.Layout.Nodes[0].GPUs).To(BeEmpty())
		Expect(r.Warnings).To(ContainElement(ContainSubstring("cannot enumerate remote hosts")))
	})

	It("warns and continues when nvidia-smi fails", func() {
		runner := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 127, Stderr: []byte("nvidia-smi: not found")}
		}}
		r := hostfileFab("node001 2 all.q@node001 0-1\n", map[string]string{"PE": "smp.pe"}, runner, nvidiaCfg())
		Expect(r.Layout.Nodes[0].GPUs).To(BeEmpty())
		Expect(r.Warnings).To(ContainElement(ContainSubstring("nvidia-smi failed")))
	})
})

// nodeSlots renders a PE_HOSTFILE line (kept for readability of multi-host cases).
func nodeSlots(host string, slots int) string {
	return host + " " + strconv.Itoa(slots) + " all.q@" + host + " 0-" + strconv.Itoa(slots-1) + "\n"
}

var _ = Describe("multi-host GPU discovery through Fabricate [REQ-GPU-001]", func() {
	It("assigns each host its own granted devices", func() {
		// Two GRU elements, one per host, distinct device sets.
		xml := `<detailed_job_info><djob_info><element>
		  <JB_job_number>267</JB_job_number>
		  <JB_ja_tasks><element>
		    <JAT_granted_resources_list>
		      <element><GRU_name>gpu</GRU_name><GRU_host>node001</GRU_host>
		        <GRU_resource_map_list><element><RESL_value>0</RESL_value></element></GRU_resource_map_list></element>
		      <element><GRU_name>gpu</GRU_name><GRU_host>node002</GRU_host>
		        <GRU_resource_map_list>
		          <element><RESL_value>0</RESL_value></element>
		          <element><RESL_value>1</RESL_value></element>
		        </GRU_resource_map_list></element>
		    </JAT_granted_resources_list>
		  </element></JB_ja_tasks>
		</element></djob_info></detailed_job_info>`
		runner := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte(xml)}
		}}
		r := hostfileFab(nodeSlots("node001", 1)+nodeSlots("node002", 2),
			map[string]string{"PE": "smp.pe"}, runner, testConfig())
		Expect(r.Layout.Nodes[0].GPUs).To(Equal([]int{0}))
		Expect(r.Layout.Nodes[1].GPUs).To(Equal([]int{0, 1}))
	})
})
