package doctor_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/cli/doctor"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

func TestDoctor(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Doctor Suite")
}

var _ = Describe("CompatNotes [version matrix]", func() {
	It("is silent on the build that carries the accounting fix and anything newer", func() {
		Expect(doctor.CompatNotes(gedata.OCSBuild{Release: "9.1.5", Stamp: "250826-0734"})).To(BeEmpty())
		Expect(doctor.CompatNotes(gedata.OCSBuild{Release: "9.2.0", Stamp: "260101-0000"})).To(BeEmpty())
	})

	It("names both degradations on 9.0.x", func() {
		notes := doctor.CompatNotes(gedata.OCSBuild{Release: "9.0.10"})
		Expect(notes).To(HaveLen(2))
		Expect(notes[0]).To(ContainSubstring("sacct ExitCode"))
		Expect(notes[0]).To(ContainSubstring("250826-0734"), "the exact build that contains the fix")
		Expect(notes[1]).To(ContainSubstring("-par"))
	})

	It("warns on an earlier 9.1.5 build that predates the fix", func() {
		Expect(doctor.CompatNotes(gedata.OCSBuild{Release: "9.1.5", Stamp: "250801-0000"})).NotTo(BeEmpty())
	})
})
