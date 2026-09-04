package launch

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

var _ = Describe("PE config parsing", func() {
	It("parses key/value pairs from real qconf -sp output", func() {
		data, err := os.ReadFile("testdata/qconf_sp_make.txt")
		Expect(err).NotTo(HaveOccurred())
		pe := ParsePEConfig(data)
		Expect(pe["pe_name"]).To(Equal("make"))
		Expect(pe["control_slaves"]).To(Equal("TRUE"))
		Expect(pe["daemon_forks_slaves"]).To(Equal("FALSE"))
		Expect(pe["allocation_rule"]).To(Equal("$round_robin"))
	})
})

var _ = Describe("launch preflight [REQ-CHN-005, SI-18]", func() {
	fixtureRunner := func() *fake.Runner {
		data, err := os.ReadFile("testdata/qconf_sp_make.txt")
		Expect(err).NotTo(HaveOccurred())
		return &fake.Runner{Responder: func(name string, args []string) fake.Response {
			Expect(name).To(Equal("qconf"))
			// The preflight also asks for the global config to locate the execd
			// spool (SI-51). Refusing it leaves the token check unable to
			// determine the path, which is the advisory this spec asserts on.
			if len(args) > 0 && args[0] == "-sconf" {
				return fake.Response{Exit: 1}
			}
			Expect(args).To(Equal([]string{"-sp", "make"}))
			return fake.Response{Stdout: data}
		}}
	}

	It("passes with control_slaves TRUE, warning on per-slot rlimits and token spool [REQ-APX-003]", func() {
		res := Preflight(context.Background(), fixtureRunner(), "make")
		Expect(res.OK()).To(BeTrue())
		Expect(res.Warnings).To(ContainElement(ContainSubstring("per-slot h_vmem")))
		Expect(res.Warnings).To(ContainElement(ContainSubstring("spool file is owner-only")))
	})

	It("fails loud when control_slaves is not TRUE", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte("pe_name make\ncontrol_slaves FALSE\ndaemon_forks_slaves FALSE\n")}
		}}
		res := Preflight(context.Background(), r, "make")
		Expect(res.OK()).To(BeFalse())
		Expect(res.Errors).To(ContainElement(ContainSubstring("control_slaves TRUE")))
	})

	It("warns about broken concurrent steps when daemon_forks_slaves is TRUE", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte("control_slaves TRUE\ndaemon_forks_slaves TRUE\n")}
		}}
		res := Preflight(context.Background(), r, "make")
		Expect(res.OK()).To(BeTrue())
		Expect(res.Warnings).To(ContainElement(ContainSubstring("concurrent srun steps will not run")))
	})

	It("fails loud when the PE config cannot be read", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qconf: PE \"make\" does not exist")}
		}}
		res := Preflight(context.Background(), r, "make")
		Expect(res.OK()).To(BeFalse())
	})

	It("is a no-op for a single-node job with no PE", func() {
		res := Preflight(context.Background(), fixtureRunner(), "")
		Expect(res.OK()).To(BeTrue())
		Expect(res.Warnings).To(BeEmpty())
	})
})

var _ = Describe("tailBuffer", func() {
	It("keeps only the last limit bytes", func() {
		tb := &tailBuffer{limit: 4}
		_, _ = tb.Write([]byte("abcdefg"))
		Expect(tb.String()).To(Equal("defg"))
	})
})
