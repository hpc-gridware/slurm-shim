package gedata_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

var _ = Describe("qstat -j requested-resource parsing [REQ-FAB-003]", func() {
	Describe("ParseRequestedResourceXML", func() {
		It("reads the requested h_vmem in bytes from CE_doubleval", func() {
			amount, ok, err := gedata.ParseRequestedResourceXML(readFixture("qstat_j_mem.xml"), "h_vmem")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			// qsub -l h_vmem=2G -> 2147483648 bytes.
			Expect(amount).To(BeNumerically("==", 2147483648))
		})

		It("reports not-requested for a complex the job did not ask for", func() {
			_, ok, err := gedata.ParseRequestedResourceXML(readFixture("qstat_j_mem.xml"), "mem_free")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("reports not-requested when the job requested no resources", func() {
			_, ok, err := gedata.ParseRequestedResourceXML(readFixture("qstat_j_gpu1.xml"), "h_vmem")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("errors on malformed XML", func() {
			_, _, err := gedata.ParseRequestedResourceXML([]byte("<detailed_job_info><nope"), "h_vmem")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("RequestedResource via the Runner", func() {
		It("runs qstat -xml -j and returns the requested bytes", func() {
			r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
				Expect(name).To(Equal("qstat"))
				Expect(args).To(Equal([]string{"-xml", "-j", "268"}))
				return fake.Response{Stdout: readFixture("qstat_j_mem.xml")}
			}}
			amount, ok, err := gedata.RequestedResource(context.Background(), r, "268", "h_vmem")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(amount).To(BeNumerically("==", 2147483648))
		})

		It("returns an error on a non-zero qstat exit", func() {
			r := &fake.Runner{Responder: func(string, []string) fake.Response {
				return fake.Response{Exit: 1, Stderr: []byte("qstat: cannot connect")}
			}}
			_, _, err := gedata.RequestedResource(context.Background(), r, "268", "h_vmem")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot connect"))
		})
	})
})
