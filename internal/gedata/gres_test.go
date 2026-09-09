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
				{Host: "ocs-worker1", Devices: []string{"0"}},
			}))
		})

		It("parses two granted devices on one host (gpu=2)", func() {
			hosts, err := gedata.ParseGrantedGPUsXML(readFixture("qstat_j_gpu2.xml"), "gpu")
			Expect(err).NotTo(HaveOccurred())
			Expect(hosts).To(Equal([]gedata.HostGPUs{
				{Host: "ocs-worker2", Devices: []string{"0", "1"}},
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
				{Host: "node-a", Devices: []string{"0", "1"}},
				{Host: "node-b", Devices: []string{"2"}},
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
				{Host: "ocs-worker2", Devices: []string{"0", "1"}},
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
			Expect(hosts).To(Equal([]gedata.HostGPUs{{Host: "ocs-worker2", Devices: []string{"0", "1"}}}))
		})
	})

	Describe("SGE_HGR_<complex> (local host only, SI-19)", func() {
		It("parses the space-separated id list the job sees", func() {
			Expect(idsOf(gedata.ParseSGEHGR("0 1"))).To(Equal([]string{"0", "1"}))
			Expect(idsOf(gedata.ParseSGEHGR("0"))).To(Equal([]string{"0"}))
			Expect(idsOf(gedata.ParseSGEHGR(""))).To(BeEmpty())
		})

		It("maps named ids by trailing number", func() {
			Expect(idsOf(gedata.ParseSGEHGR("gpu2 gpu3"))).To(Equal([]string{"2", "3"}))
		})

		It("falls back to ordinal position for ids with no number", func() {
			Expect(idsOf(gedata.ParseSGEHGR("gpuA gpuB"))).To(Equal([]string{"0", "1"}))
			// Mixed: numeric-in-token wins over ordinal where present.
			Expect(idsOf(gedata.ParseSGEHGR("gpuA gpu5"))).To(Equal([]string{"0", "5"}))
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
			Expect(hosts).To(Equal([]gedata.HostGPUs{{Host: "ocs-worker1", Devices: []string{"0"}}}))
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

var _ = Describe("device token normalisation [REQ-GPU-002]", func() {
	DescribeTable("DeviceToken",
		func(token string, ordinal int, want string) {
			got, _ := gedata.DeviceToken(token, ordinal)
			Expect(got).To(Equal(want))
		},
		// Numeric ids are the long-standing contract and pass through untouched,
		// so every existing site is byte-identical after the widening.
		Entry("numeric id", "0", 0, "0"),
		// Canonicalised, as the old int-based parser did implicitly. ROCr rejects
		// a token that is not the canonical rendering of its index and discards
		// every id to its right, so "007" would hand the job zero devices.
		Entry("zero-padded numeric id", "007", 0, "7"),
		Entry("explicitly signed numeric id", "+3", 0, "3"),
		Entry("numeric id out of order", "5", 0, "5"),
		Entry("surrounding space", " 3 ", 0, "3"),
		// The reason the type widened: a UUID is the only device identity that
		// survives a reboot, a driver reload, or an AMD partition-mode change.
		// It used to collapse to its ordinal position and select a wrong device.
		Entry("AMD UUID", "GPU-0123456789abcdef", 0, "GPU-0123456789abcdef"),
		Entry("AMD UUID at a later position", "GPU-dead0000000000ff", 1, "GPU-dead0000000000ff"),
		Entry("UUID case is preserved verbatim", "gpu-ABCDEF0123456789", 0, "gpu-ABCDEF0123456789"),
		Entry("NVIDIA dashed UUID", "GPU-4b2c1a9f-8d3e-6f7a-b5c9-2e4d8a1f6c3b", 0,
			"GPU-4b2c1a9f-8d3e-6f7a-b5c9-2e4d8a1f6c3b"),
		// Named ids keep the historical trailing-digit coercion.
		Entry("named id", "gpu0", 0, "0"),
		Entry("named id with a larger number", "gpu12", 0, "12"),
		// Indistinguishable from a named id by shape, and coerced the same way it
		// always has been. Documented rather than special-cased.
		Entry("a name that merely looks like one", "renderD128", 0, "128"),
		Entry("zero-padded named id", "gpu01", 0, "1"),
		// A hyphenated NAME is not a UUID. These were working ids before device
		// ids became strings, and passing them through verbatim would match no
		// device at all.
		Entry("hyphenated name", "gpu-0", 0, "0"),
		Entry("hyphenated name, second device", "gpu-1", 0, "1"),
		Entry("upper-case hyphenated name", "GPU-1", 0, "1"),
		// Last resort: usable, but a guess.
		Entry("unrecognised token falls back to its position", "wat", 3, "3"),
	)

	DescribeTable("DeviceToken reports whether an id was understood",
		func(token string, want bool) {
			_, ok := gedata.DeviceToken(token, 7777)
			Expect(ok).To(Equal(want))
		},
		Entry("numeric", "0", true),
		Entry("uuid", "GPU-0123456789abcdef", true),
		Entry("named", "gpu3", true),
		Entry("nonsense", "wat", false),
		// Ids that merely END in digits are guesses, not understood ids. Coercing
		// them silently is how a device identity becomes the wrong device.
		Entry("MIG instance id", "MIG-GPU-0123456789abcdef/13/0", false),
		Entry("PCI address", "0000:c1:00.0", false),
		Entry("comma-separated pair", "gpu0,1", false),
		Entry("embedded assignment", "a=b3", false),
		Entry("empty", "", false),
	)

	It("keeps a UUID grant intact through the XML parser", func() {
		// The regression this whole phase exists for: before the widening these
		// two UUIDs became 0 and 1, so a grant of two specific devices silently
		// became "the first two devices on the node".
		xml := `<detailed_job_info><djob_info><element>
		  <JB_job_number>900</JB_job_number>
		  <JB_ja_tasks><element>
		    <JAT_granted_resources_list>
		      <element><GRU_name>gpu</GRU_name><GRU_host>node001</GRU_host>
		        <GRU_resource_map_list>
		          <element><RESL_value>GPU-0123456789abcdef</RESL_value><RESL_amount>1</RESL_amount></element>
		          <element><RESL_value>GPU-dead0000000000ff</RESL_value><RESL_amount>1</RESL_amount></element>
		        </GRU_resource_map_list></element>
		    </JAT_granted_resources_list>
		  </element></JB_ja_tasks>
		</element></djob_info></detailed_job_info>`
		got, err := gedata.ParseGrantedGPUsXML([]byte(xml), "gpu")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].Devices).To(Equal([]string{"GPU-0123456789abcdef", "GPU-dead0000000000ff"}))
	})

	It("keeps a UUID grant intact through the plain-text fallback", func() {
		text := "resource_map  1:  gpu=node001=(GPU-0123456789abcdef GPU-dead0000000000ff)"
		got := gedata.ParseResourceMapPlain(text, "gpu")
		Expect(got).To(HaveLen(1))
		Expect(got[0].Devices).To(Equal([]string{"GPU-0123456789abcdef", "GPU-dead0000000000ff"}))
	})
})

var _ = Describe("unrecognised device ids are reported, not hidden [REQ-GPU-002]", func() {
	It("flags a granted id it could not classify", func() {
		xml := `<detailed_job_info><djob_info><element>
		  <JB_ja_tasks><element>
		    <JAT_granted_resources_list>
		      <element><GRU_name>gpu</GRU_name><GRU_host>node001</GRU_host>
		        <GRU_resource_map_list>
		          <element><RESL_value>0</RESL_value><RESL_amount>1</RESL_amount></element>
		          <element><RESL_value>wat</RESL_value><RESL_amount>1</RESL_amount></element>
		        </GRU_resource_map_list></element>
		    </JAT_granted_resources_list>
		  </element></JB_ja_tasks>
		</element></djob_info></detailed_job_info>`
		got, err := gedata.ParseGrantedGPUsXML([]byte(xml), "gpu")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		// Still usable: the id became its grant position so the job can run.
		Expect(got[0].Devices).To(Equal([]string{"0", "1"}))
		// But the caller is told, because position is a guess.
		Expect(got[0].Unrecognized).To(Equal([]string{"wat"}))
	})

	It("reports nothing for ids it understands", func() {
		text := "resource_map  1:  gpu=node001=(0 GPU-0123456789abcdef gpu3)"
		got := gedata.ParseResourceMapPlain(text, "gpu")
		Expect(got).To(HaveLen(1))
		Expect(got[0].Devices).To(Equal([]string{"0", "GPU-0123456789abcdef", "3"}))
		Expect(got[0].Unrecognized).To(BeEmpty())
	})

	It("flags an unclassifiable id in the plain-text path too", func() {
		got := gedata.ParseResourceMapPlain("resource_map  1:  gpu=node001=(0 wat)", "gpu")
		Expect(got).To(HaveLen(1))
		Expect(got[0].Unrecognized).To(Equal([]string{"wat"}))
	})
})

var _ = Describe("device id classification cannot drift from normalisation", func() {
	// One function returns both facts, so this pins the invariant that ties them:
	// an id reported as understood must never have been replaced by its position.
	// A distinctive ordinal keeps a token that merely contains those digits from
	// producing a false match.
	const ordinal = 7777

	DescribeTable("an understood id never falls back to its grant position",
		func(token string) {
			id, ok := gedata.DeviceToken(token, ordinal)
			Expect(ok).To(BeTrue(), "expected %q to be understood", token)
			Expect(id).NotTo(Equal("7777"))
		},
		Entry("numeric", "3"),
		Entry("padded numeric", "007"),
		Entry("uuid", "GPU-0123456789abcdef"),
		Entry("named", "gpu2"),
		Entry("hyphenated name", "gpu-2"),
	)

	DescribeTable("an id that was not understood always falls back to its position",
		func(token string) {
			id, ok := gedata.DeviceToken(token, ordinal)
			Expect(ok).To(BeFalse(), "expected %q not to be understood", token)
			Expect(id).To(Equal("7777"))
		},
		Entry("nonsense", "wat"),
		Entry("empty", ""),
		Entry("MIG id", "MIG-GPU-0123456789abcdef/13/0"),
		Entry("PCI address", "0000:c1:00.0"),
	)
})

// idsOf drops the "unrecognized" half of a ParseSGEHGR result so a spec can
// assert on the ids alone.
func idsOf(ids, _ []string) []string { return ids }
