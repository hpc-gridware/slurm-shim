package gedata_test

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func readFixture(name string) []byte {
	data, err := os.ReadFile("testdata/" + name)
	Expect(err).NotTo(HaveOccurred())
	return data
}

var _ = Describe("granted GPU parsing [REQ-GPU-001]", func() {
	Describe("qstat -xml -j (structured, host-qualified)", func() {
		It("parses a single granted device (gpu=1)", func() {
			hosts, err := gedata.ParseGrantedGPUsXML(readFixture("qstat_j_gpu1.xml"), "gpu")
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts).To(Equal([]gedata.HostGPUs{
				{Host: "ocs-worker1", Devices: []int{0}},
			}))
		})

		It("parses two granted devices on one host (gpu=2)", func() {
			hosts, err := gedata.ParseGrantedGPUsXML(readFixture("qstat_j_gpu2.xml"), "gpu")
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts).To(Equal([]gedata.HostGPUs{
				{Host: "ocs-worker2", Devices: []int{0, 1}},
			}))
		})

		It("ignores resources whose complex name does not match", func() {
			hosts, err := gedata.ParseGrantedGPUsXML(readFixture("qstat_j_gpu2.xml"), "mps")
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts).To(BeEmpty())
		})

		It("returns empty for a job with no granted resources", func() {
			hosts, err := gedata.ParseGrantedGPUsXML(readFixture("qstat_j_mem.xml"), "gpu")
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts).To(BeEmpty())
		})

		It("aggregates one element per host for a multi-host job", func() {
			// Synthetic two-host grant: the schema emits one <element> per host.
			xml := `<detailed_job_info><djob_info><element>
			  <JB_job_number>900</JB_job_number>
			  <JB_ja_tasks><element>
			    <JAT_granted_resources_list>
			      <element><GRU_name>gpu</GRU_name><GRU_host>node-a</GRU_host>
			        <GRU_resource_map_list>
			          <element><RESL_value>0</RESL_value><RESL_amount>1</RESL_amount></element>
			          <element><RESL_value>1</RESL_value><RESL_amount>1</RESL_amount></element>
			        </GRU_resource_map_list></element>
			      <element><GRU_name>gpu</GRU_name><GRU_host>node-b</GRU_host>
			        <GRU_resource_map_list>
			          <element><RESL_value>2</RESL_value><RESL_amount>1</RESL_amount></element>
			        </GRU_resource_map_list></element>
			    </JAT_granted_resources_list>
			  </element></JB_ja_tasks>
			</element></djob_info></detailed_job_info>`
			hosts, err := gedata.ParseGrantedGPUsXML([]byte(xml), "gpu")
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts).To(Equal([]gedata.HostGPUs{
				{Host: "node-a", Devices: []int{0, 1}},
				{Host: "node-b", Devices: []int{2}},
			}))
		})

		It("errors on malformed XML", func() {
			_, err := gedata.ParseGrantedGPUsXML([]byte(`<detailed_job_info><not closed`), "gpu")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("qstat -j plain (fallback)", func() {
		It("parses the flattened resource_map line", func() {
			hosts := gedata.ParseResourceMapPlain(string(readFixture("qstat_j_gpu2_plain.txt")), "gpu")
			Expect(hosts).To(Equal([]gedata.HostGPUs{
				{Host: "ocs-worker2", Devices: []int{0, 1}},
			}))
		})

		It("ignores non-matching complexes", func() {
			hosts := gedata.ParseResourceMapPlain(string(readFixture("qstat_j_gpu1_plain.txt")), "mps")
			Expect(hosts).To(BeEmpty())
		})

		It("runs qstat -j and parses its plain output via GrantedGPUsPlain", func() {
			r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
				Expect(args).To(Equal([]string{"-j", "267"}))
				return fake.Response{Stdout: readFixture("qstat_j_gpu2_plain.txt")}
			}}
			hosts, err := gedata.GrantedGPUsPlain(context.Background(), r, "267", "gpu")
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts).To(Equal([]gedata.HostGPUs{{Host: "ocs-worker2", Devices: []int{0, 1}}}))
		})
	})

	Describe("SGE_HGR_<complex> (local host only, SI-19)", func() {
		It("parses the space-separated id list the job sees", func() {
			Expect(gedata.ParseSGEHGR("0 1")).To(Equal([]int{0, 1}))
			Expect(gedata.ParseSGEHGR("0")).To(Equal([]int{0}))
			Expect(gedata.ParseSGEHGR("")).To(BeEmpty())
		})

		It("maps named ids by trailing number", func() {
			Expect(gedata.ParseSGEHGR("gpu2 gpu3")).To(Equal([]int{2, 3}))
		})

		It("falls back to ordinal position for ids with no number", func() {
			Expect(gedata.ParseSGEHGR("gpuA gpuB")).To(Equal([]int{0, 1}))
			// Mixed: numeric-in-token wins over ordinal where present.
			Expect(gedata.ParseSGEHGR("gpuA gpu5")).To(Equal([]int{0, 5}))
		})
	})

	Describe("GrantedGPUs via the Runner", func() {
		It("runs qstat -xml -j and parses its output", func() {
			r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
				Expect(name).To(Equal("qstat"))
				Expect(args).To(Equal([]string{"-xml", "-j", "266"}))
				return fake.Response{Stdout: readFixture("qstat_j_gpu1.xml")}
			}}
			hosts, err := gedata.GrantedGPUs(context.Background(), r, "266", "gpu")
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts).To(Equal([]gedata.HostGPUs{{Host: "ocs-worker1", Devices: []int{0}}}))
		})

		It("returns an error on a non-zero qstat exit", func() {
			r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
				return fake.Response{Exit: 1, Stderr: []byte("qstat: cannot connect")}
			}}
			_, err := gedata.GrantedGPUs(context.Background(), r, "266", "gpu")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot connect"))
		})
	})
})
