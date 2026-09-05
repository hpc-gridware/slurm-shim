package gedata_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// The real `qconf -sc` table shape, including the comment header GE emits.
const scTable = `#name               shortcut   type        relop requestable consumable default  urgency
#----------------------------------------------------------------------------------------
h_vmem              h_vmem     MEMORY      <=    YES         NO         0        0
mem_free            mf         MEMORY      <=    YES         NO         0        0
m_mem_free          mfr        MEMORY      <=    YES         YES        0        0
gpu                 gpu        RSMAP       <=    YES         HOST       0        0
slots               s          INT         <=    YES         JOB        1        1000
`

func scRunner() *fake.Runner {
	return &fake.Runner{Responder: func(string, []string) fake.Response {
		return fake.Response{Stdout: []byte(scTable)}
	}}
}

var _ = Describe("ConsumableScope [qconf -sc]", func() {
	It("reads each scope Grid Engine can report", func() {
		for name, want := range map[string]string{
			"h_vmem": "NO", "mem_free": "NO", "m_mem_free": "YES",
			"gpu": "HOST", "slots": "JOB",
		} {
			got, err := gedata.ConsumableScope(context.Background(), scRunner(), name)
			Expect(err).NotTo(HaveOccurred(), name)
			Expect(got).To(Equal(want), name)
		}
	})

	It("accepts a complex's shortcut as well as its name", func() {
		got, err := gedata.ConsumableScope(context.Background(), scRunner(), "mf")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("NO"))
	})

	It("errors for a complex the cluster does not define", func() {
		_, err := gedata.ConsumableScope(context.Background(), scRunner(), "nope")
		Expect(err).To(HaveOccurred())
	})

	It("errors when qconf fails rather than guessing a scope", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1}
		}}
		_, err := gedata.ConsumableScope(context.Background(), r, "mem_free")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("PerNodeMultiplier", func() {
	It("multiplies by slots only for a per-slot consumable", func() {
		Expect(gedata.PerNodeMultiplier("YES", 4)).To(Equal(4))
		for _, scope := range []string{"NO", "JOB", "HOST", ""} {
			Expect(gedata.PerNodeMultiplier(scope, 4)).To(Equal(1), "scope %q", scope)
		}
	})
})
