package dryrun

import (
	"bytes"
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func TestDryRun(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DryRun Suite")
}

var _ = Describe("SLURM_SHIM_DRY_RUN parsing [REQ-DRY-001]", func() {
	It("is on only for an explicit on-spelling", func() {
		for _, v := range []string{"1", "true", "TRUE", "yes", "y", "on", " on "} {
			Expect(On(v)).To(BeTrue(), "value %q should be on", v)
		}
	})

	// The mode's on-state suppresses work, so an unrecognized value must fail open
	// to "do the real thing". The reverse polarity made SLURM_SHIM_DRY_RUN=n -- the
	// likeliest spelling of "no" -- silently turn every job into a no-op.
	It("is off for every off-spelling and for anything unrecognized", func() {
		for _, v := range []string{
			"", "0", "false", "FALSE", "no", "n", "off", " off ",
			"none", "disable", "disabled", "please", "maybe",
		} {
			Expect(On(v)).To(BeFalse(), "value %q should be off", v)
		}
	})

	It("reports a value that is neither an on- nor an off-spelling", func() {
		Expect(unrecognized("maybe")).To(Equal("maybe"))
		Expect(unrecognized("please")).To(Equal("please"))
		for _, v := range []string{"", "0", "no", "n", "off", "1", "true", "yes", "y", "on"} {
			Expect(unrecognized(v)).To(BeEmpty(), "value %q is understood", v)
		}
	})
})

var _ = Describe("control character escaping", func() {
	It("renders C0 controls printable so report text cannot repaint a terminal", func() {
		Expect(Escape("aaa\x1b[2K\rqsub")).To(Equal(`aaa\x1b[2K\rqsub`))
		Expect(Escape("a\tb\nc")).To(Equal(`a\tb\nc`))
		Expect(Escape("plain")).To(Equal("plain"))
	})

	It("escapes controls inside a quoted token", func() {
		Expect(Quote("job\x1bname")).To(Equal(`'job\x1bname'`))
	})
})

var _ = Describe("assignment redaction", func() {
	It("keeps the key and masks the value", func() {
		// Quoted because the placeholder's angle brackets are shell-special.
		Expect(RedactAssignment("HF_TOKEN=hf_liveSecret123")).To(Equal(`'HF_TOKEN=<value>'`))
	})

	It("leaves a token with no assignment alone", func() {
		Expect(RedactAssignment("-V")).To(Equal("-V"))
	})
})

var _ = Describe("command rendering", func() {
	It("leaves shell-safe tokens unquoted", func() {
		Expect(Command("qsub", []string{"-terse", "-q", "all.q", "/home/u/job.sh"})).
			To(Equal("qsub -terse -q all.q /home/u/job.sh"))
	})

	It("quotes tokens a shell would interpret", func() {
		Expect(Command("qsub", []string{"-o", "slurm-$JOB_ID.out", "-N", "my job"})).
			To(Equal(`qsub -o 'slurm-$JOB_ID.out' -N 'my job'`))
	})

	// The command name is user-controlled for srun, and an unquoted one made the
	// rendered line run a second command when pasted.
	It("quotes the command name too", func() {
		Expect(Command("my script; rm -rf ~", []string{"--flag"})).
			To(Equal(`'my script; rm -rf ~' --flag`))
	})

	It("escapes an embedded single quote", func() {
		Expect(Quote(`it's`)).To(Equal(`'it'\''s'`))
	})

	It("renders an empty argument visibly", func() {
		Expect(Quote("")).To(Equal("''"))
	})
})

var _ = Describe("Runner interception [REQ-DRY-002]", func() {
	var (
		inner *fake.Runner
		out   *bytes.Buffer
		r     Runner
	)

	BeforeEach(func() {
		inner = &fake.Runner{}
		out = &bytes.Buffer{}
		r = Runner{Inner: inner, Out: out, Prefix: "scancel"}
	})

	It("reports a mutating client instead of running it", func() {
		_, _, exit, err := r.Run(context.Background(), "qdel", "4711", "-t", "3")

		Expect(err).NotTo(HaveOccurred())
		Expect(exit).To(Equal(0))
		Expect(inner.Calls).To(BeEmpty(), "qdel must not reach the cluster")
		Expect(out.String()).To(Equal("scancel: dry run: would run: qdel 4711 -t 3\n"))
	})

	It("intercepts every mutating client, reporting each", func() {
		for name := range mutating {
			out.Reset()
			_, _, exit, err := r.Run(context.Background(), name, "x")
			Expect(err).NotTo(HaveOccurred())
			Expect(exit).To(Equal(0))
			Expect(out.String()).To(Equal("scancel: dry run: would run: " + name + " x\n"))
		}
		Expect(inner.Calls).To(BeEmpty())
	})

	// The map is the intercept set for clients reachable through gedata.Runner.
	// qrsh mutates job state but is exec'd directly by internal/launch, so it
	// cannot be intercepted here -- srun's explicit branch is what guards it.
	It("covers exactly the mutating clients the shim invokes through Runner", func() {
		Expect(mutating).To(HaveKey("qsub"))
		Expect(mutating).To(HaveKey("qdel"))
		Expect(mutating).To(HaveKey("qmod"))
		Expect(mutating).NotTo(HaveKey("qstat"))
		Expect(mutating).NotTo(HaveKey("qconf"))
		Expect(mutating).NotTo(HaveKey("qacct"))
	})

	It("passes read-only clients through so state can still be resolved", func() {
		inner.Responder = func(string, []string) fake.Response {
			return fake.Response{Stdout: []byte("<job_info/>")}
		}
		stdout, _, _, err := r.Run(context.Background(), "qstat", "-xml")

		Expect(err).NotTo(HaveOccurred())
		Expect(string(stdout)).To(Equal("<job_info/>"))
		Expect(inner.Calls).To(HaveLen(1))
		Expect(inner.Calls[0].Name).To(Equal("qstat"))
		Expect(out.String()).To(BeEmpty())
	})

	It("returns the runner unwrapped when the mode is off", func() {
		GinkgoT().Setenv(EnvVar, "0")
		Expect(Wrap(inner, out, "scancel")).To(BeIdenticalTo(inner))
	})

	It("wraps the runner when the mode is on", func() {
		GinkgoT().Setenv(EnvVar, "1")
		Expect(Wrap(inner, out, "scancel")).NotTo(BeIdenticalTo(inner))
	})
})
