package config_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

var _ = Describe("MergeInto [todo 080]", func() {
	// The defect this prevents: `install --apply` rendered the Config struct over
	// the whole file, so any key the running binary did not model was silently
	// dropped. Observed in the field -- an older binary erased `gpu.vendor` from a
	// site config and the cluster kept running without it.
	It("preserves a TOP-LEVEL key the struct does not model", func() {
		existing := "compat_version: \"24.05.0\"\nsite_only_key: keep-me\n"
		out, err := config.MergeInto([]byte(existing), config.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring("site_only_key: keep-me"))
	})

	It("preserves a NESTED key the struct does not model", func() {
		// The real case was nested: `vendor` under `gpu:`, not a top-level key.
		// A non-recursive merge would still have lost it.
		existing := "gpu:\n  isolation: shim\n  future_key: keep-me\n"
		out, err := config.MergeInto([]byte(existing), config.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring("future_key: keep-me"))
	})

	It("still applies the installer's own values", func() {
		cfg := config.Default()
		cfg.CompatVersion = "25.05.0"
		out, err := config.MergeInto([]byte("compat_version: \"24.05.0\"\nextra: x\n"), cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring("25.05.0"))
		Expect(string(out)).NotTo(ContainSubstring("24.05.0"))
		Expect(string(out)).To(ContainSubstring("extra: x"))
	})

	It("round-trips: the merged document still parses, keeping both halves", func() {
		// The strongest form of the claim -- not just "the text is present" but
		// "the result is a usable config that still warns about the extra key".
		existing := "gpu:\n  isolation: shim\n  future_key: keep-me\nsite_only_key: v\n"
		out, err := config.MergeInto([]byte(existing), config.Default())
		Expect(err).NotTo(HaveOccurred())
		cfg, warns, perr := config.Parse(out)
		Expect(perr).NotTo(HaveOccurred())
		Expect(cfg).NotTo(BeNil())
		Expect(warns).To(ContainElement(ContainSubstring("site_only_key")))
	})

	It("writes the plain rendering when there is no existing file", func() {
		out, err := config.MergeInto(nil, config.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(BeEmpty())
		_, _, perr := config.Parse(out)
		Expect(perr).NotTo(HaveOccurred())
	})

	It("treats a whitespace-only file as empty", func() {
		out, err := config.MergeInto([]byte("\n  \n"), config.Default())
		Expect(err).NotTo(HaveOccurred())
		_, _, perr := config.Parse(out)
		Expect(perr).NotTo(HaveOccurred())
	})

	It("REFUSES to overwrite a file it cannot parse", func() {
		// Destroying a malformed-but-intentional config is the one outcome this
		// function exists to prevent, so it errors rather than falling back to a
		// wholesale write.
		_, err := config.MergeInto([]byte("gpu: [unclosed\n"), config.Default())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not valid YAML"))
	})

	It("keeps comments on keys it does not own", func() {
		existing := "# site note\nsite_only_key: v\n"
		out, err := config.MergeInto([]byte(existing), config.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring("# site note"))
	})

	It("is idempotent: merging twice changes nothing further", func() {
		existing := "gpu:\n  future_key: keep-me\n"
		once, err := config.MergeInto([]byte(existing), config.Default())
		Expect(err).NotTo(HaveOccurred())
		twice, err := config.MergeInto(once, config.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(twice))).To(Equal(strings.TrimSpace(string(once))))
	})
})

var _ = Describe("MergeInto comment handling [todo 080]", func() {
	It("keeps an inline comment on a key whose VALUE the installer rewrites", func() {
		// `vendor` IS modelled, so its value is replaced -- but the operator's
		// note beside it explains WHY the value is what it is, and deleting that
		// on every install is its own small data loss.
		existing := "gpu:\n  vendor: nvidia # deliberate: this cluster is NVIDIA\n"
		out, err := config.MergeInto([]byte(existing), config.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring("deliberate: this cluster is NVIDIA"))
	})
})

var _ = Describe("MergeInto and retired keys [todo 080 + 079]", func() {
	// The regression these guard against only exists when 079 and 080 are both
	// in: 079 makes launch_ramp unknown, 080 makes unknown keys permanent. Parse
	// runs on every command, so the pair turns one stale key into a warning line
	// on every srun, sbatch, squeue and sinfo invocation, forever.
	It("DROPS a key the shim retired, rather than preserving it forever", func() {
		// Exactly what a pre-079 `install --apply` wrote: both keys had
		// non-omitempty defaults, so every existing config file contains them.
		in := []byte("compat_version: \"24.05.0\"\nlaunch_ramp: 64\ncontrol_port: \"\"\n")
		out, err := config.MergeInto(in, config.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).NotTo(ContainSubstring("launch_ramp"))
		Expect(string(out)).NotTo(ContainSubstring("control_port:"))
	})

	It("leaves the merged file warning-free, which is the whole point", func() {
		in := []byte("launch_ramp: 64\ngpu:\n  vendor: nvidia\n")
		out, err := config.MergeInto(in, config.Default())
		Expect(err).NotTo(HaveOccurred())
		_, warns, err := config.Parse(out)
		Expect(err).NotTo(HaveOccurred())
		for _, w := range warns {
			Expect(w).NotTo(ContainSubstring("launch_ramp"))
		}
	})

	It("still preserves an unknown key that is NOT retired", func() {
		// The retirement list must stay a narrow exception: a key the shim does
		// not recognize may be a site setting a newer binary models.
		in := []byte("launch_ramp: 64\nsite_tuning: please-keep\n")
		out, err := config.MergeInto(in, config.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring("site_tuning: please-keep"))
		Expect(string(out)).NotTo(ContainSubstring("launch_ramp"))
	})

	It("does not drop a NESTED key that happens to share a retired name", func() {
		// Retirement is a top-level fact. A sub-key called launch_ramp under an
		// unrelated mapping is somebody else's data.
		in := []byte("gpu:\n  vendor: nvidia\n  launch_ramp: 8\n")
		out, err := config.MergeInto(in, config.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring("launch_ramp: 8"))
	})
})
