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
	// queueCfg is what `qconf -sq` returns. "INFINITY" everywhere is the GE
	// default and means the per-slot hazard cannot bite.
	unlimited := "qname                 all.q\nh_vmem               INFINITY\ns_vmem               INFINITY\n"
	capped := "qname                 all.q\nh_vmem               4G\ns_vmem               INFINITY\n"

	runnerWith := func(queueCfg string) *fake.Runner {
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
			if len(args) > 0 && args[0] == "-sq" {
				return fake.Response{Stdout: []byte(queueCfg)}
			}
			Expect(args).To(Equal([]string{"-sp", "make"}))
			return fake.Response{Stdout: data}
		}}
	}
	fixtureRunner := func() *fake.Runner { return runnerWith(unlimited) }

	It("passes with control_slaves TRUE [REQ-APX-003]", func() {
		res := Preflight(context.Background(), fixtureRunner(), "make", "all.q")
		Expect(res.OK()).To(BeTrue())
	})

	It("warns about the per-slot hazard ONLY when the queue really caps memory", func() {
		// The point of SI-18 is an OOM under a per-slot memory cap. With every
		// limit at INFINITY there is nothing to exceed, so there is nothing to
		// say -- and saying it anyway, on every step of every job, is how a
		// warning stops being read.
		res := Preflight(context.Background(), runnerWith(capped), "make", "all.q")
		Expect(res.Warnings).To(ContainElement(ContainSubstring("SI-18")))
		Expect(res.Warnings).To(ContainElement(ContainSubstring("h_vmem=4G")),
			"the warning must name the limit it found, not just assert a hazard")
	})

	It("is SILENT when the queue imposes no per-slot memory limit", func() {
		res := Preflight(context.Background(), runnerWith(unlimited), "make", "all.q")
		for _, w := range res.Warnings {
			Expect(w).NotTo(ContainSubstring("SI-18"))
			Expect(w).NotTo(ContainSubstring("daemon_forks_slaves"))
		}
	})

	It("is SILENT when no queue is known, rather than guessing", func() {
		res := Preflight(context.Background(), fixtureRunner(), "make", "")
		for _, w := range res.Warnings {
			Expect(w).NotTo(ContainSubstring("SI-18"))
		}
	})

	It("describes each side of the tradeoff; when to print it is the caller's call", func() {
		Expect(PEForksNote(false, "make")).To(ContainSubstring("SI-18"))
		Expect(PEForksNote(true, "make")).To(ContainSubstring("concurrent srun steps will not run"))
	})

	It("does NOT report the spool exposure, which srun would print on every step", func() {
		// SI-51 is a property of the CLUSTER, not of the job: the user running
		// the job generally cannot chmod the execd spool. Preflight warnings are
		// printed by srun per step, so reporting it here told the wrong person
		// repeatedly -- and the line landed interleaved with the job own output,
		// corrupting what tools parse. doctor reports it once, under security,
		// via TokenSpoolWarning.
		res := Preflight(context.Background(), fixtureRunner(), "make", "all.q")
		for _, w := range res.Warnings {
			Expect(w).NotTo(ContainSubstring("SI-51"))
			Expect(w).NotTo(ContainSubstring("spool"))
		}
	})

	It("still exposes the spool check for doctor to call directly", func() {
		// The check itself must keep working -- only its delivery changed.
		Expect(TokenSpoolWarning(context.Background(), fixtureRunner())).
			To(ContainSubstring("SI-51"))
	})

	It("fails loud when control_slaves is not TRUE", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte("pe_name make\ncontrol_slaves FALSE\ndaemon_forks_slaves FALSE\n")}
		}}
		res := Preflight(context.Background(), r, "make", "all.q")
		Expect(res.OK()).To(BeFalse())
		Expect(res.Errors).To(ContainElement(ContainSubstring("control_slaves TRUE")))
	})

	It("warns about broken concurrent steps when daemon_forks_slaves is TRUE", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte("control_slaves TRUE\ndaemon_forks_slaves TRUE\n")}
		}}
		res := Preflight(context.Background(), r, "make", "all.q")
		Expect(res.OK()).To(BeTrue())
		Expect(res.Warnings).To(ContainElement(ContainSubstring("concurrent srun steps will not run")))
	})

	It("fails loud when the PE config cannot be read", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qconf: PE \"make\" does not exist")}
		}}
		res := Preflight(context.Background(), r, "make", "all.q")
		Expect(res.OK()).To(BeFalse())
	})

	It("is a no-op for a single-node job with no PE", func() {
		res := Preflight(context.Background(), fixtureRunner(), "", "all.q")
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
