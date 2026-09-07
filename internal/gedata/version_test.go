package gedata_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func helpRunner(banner string) *fake.Runner {
	return &fake.Runner{Responder: func(string, []string) fake.Response {
		return fake.Response{Stdout: []byte(banner + "\nusage: qconf [options]\n")}
	}}
}

var _ = Describe("OCSVersion", func() {
	It("parses the OCS banner with its build stamp", func() {
		b, err := gedata.OCSVersion(context.Background(), helpRunner("OCS 9.1.5 (250826-0734)"))
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Release).To(Equal("9.1.5"))
		Expect(b.Stamp).To(Equal("250826-0734"))
		Expect(b.String()).To(Equal("9.1.5 (250826-0734)"))
	})

	It("parses GCS and classic SGE banners without a stamp", func() {
		b, err := gedata.OCSVersion(context.Background(), helpRunner("GCS 9.1.5 (250826-0734)"))
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Release).To(Equal("9.1.5"))
		b, err = gedata.OCSVersion(context.Background(), helpRunner("SGE 8.1.9"))
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Release).To(Equal("8.1.9"))
		Expect(b.Stamp).To(BeEmpty())
	})

	It("errors on output with no version line", func() {
		_, err := gedata.OCSVersion(context.Background(), helpRunner("garbage"))
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("OCSBuild.AtLeast [gating on the build that carries a fix]", func() {
	fix := func(rel, stamp string) gedata.OCSBuild { return gedata.OCSBuild{Release: rel, Stamp: stamp} }

	It("compares releases numerically, not lexically", func() {
		Expect(fix("9.1.10", "").AtLeast("9.1.5", "")).To(BeTrue())
		Expect(fix("9.0.10", "").AtLeast("9.1.5", "")).To(BeFalse())
		Expect(fix("10.0.0", "").AtLeast("9.1.5", "")).To(BeTrue())
	})

	It("uses the build stamp only when the release is equal", func() {
		Expect(fix("9.1.5", "250826-0734").AtLeast("9.1.5", "250826-0734")).To(BeTrue())
		Expect(fix("9.1.5", "250801-1200").AtLeast("9.1.5", "250826-0734")).To(BeFalse(), "an earlier 9.1.5 build lacks the fix")
		Expect(fix("9.1.6", "250101-0000").AtLeast("9.1.5", "250826-0734")).To(BeTrue(), "a later release always has it")
	})

	It("treats a missing stamp as satisfied at the same release", func() {
		Expect(fix("9.1.5", "").AtLeast("9.1.5", "250826-0734")).To(BeTrue())
	})
})
