package srun_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"github.com/onsi/gomega/types"

	"github.com/hpc-gridware/slurm-shim/internal/layout"
)

// These drive the real srun binary, because the failure this feature exists to
// prevent lives in the interaction between the inherited job environment and the
// per-rank overlay -- not in either one alone. A unit test on the overlay passes
// while the rank is still broken.
// rankBlock returns just the lines the report attributes to one rank. The naive
// pattern `(?s)rank N \(host\) adds:.*VAR=v` spans the whole remainder of the
// report, so it can be satisfied by a different rank's block entirely.
func rankBlock(out string, rank int, host string) string {
	header := fmt.Sprintf("rank %d (%s) adds:", rank, host)
	i := strings.Index(out, header)
	if i < 0 {
		return ""
	}
	rest := out[i+len(header):]
	if j := strings.Index(rest, "\nrank "); j >= 0 {
		return rest[:j]
	}
	return rest
}

// haveEnv matches one whole KEY=VALUE line, so "=0" cannot pass for "=0,1".
func haveEnv(kv string) types.GomegaMatcher { return ContainSubstring("\n  " + kv + "\n") }

var _ = Describe("AMD device visibility end to end [REQ-GPU-004]", func() {
	// oneNodeWithGPUs is a single-host allocation holding a two-device grant.
	oneNodeWithGPUs := func() string {
		return writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true, GPUs: []string{"0", "1"}},
		}, []int{2})
	}

	writeCfg := func(tmp, body string) {
		Expect(os.WriteFile(filepath.Join(tmp, "config.yaml"), []byte(body), 0o600)).To(Succeed())
	}

	// A device variable the job already carried, as a site prolog or container
	// image would leave it. The ids differ from the grant so a leak is visible.
	inherited := []string{
		"CUDA_VISIBLE_DEVICES=6,7",
		"HIP_VISIBLE_DEVICES=6,7",
		"GPU_DEVICE_ORDINAL=6,7",
	}

	It("publishes ROCR_VISIBLE_DEVICES and removes the inherited HIP-level masks", func() {
		tmp := oneNodeWithGPUs()
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: amd\n")

		sess := runSrunEnv(tmp, append(append([]string{}, dryEnv...), inherited...),
			"-n", "2", "--gpus-per-task=1", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := errOf(sess)

		// Each rank gets its own device, under the ROCr name.
		Expect(rankBlock(out, 0, "node001")).To(haveEnv("ROCR_VISIBLE_DEVICES=0"))
		Expect(rankBlock(out, 1, "node001")).To(haveEnv("ROCR_VISIBLE_DEVICES=1"))

		// The whole point: the inherited masks are gone from the shared rank
		// environment. Left in place they would layer on top of the ROCr mask and
		// silently select the wrong devices, since the HIP-level variables index
		// into the list ROCR_VISIBLE_DEVICES has already filtered and renumbered.
		Expect(out).NotTo(ContainSubstring("CUDA_VISIBLE_DEVICES=6,7"))
		// Removal, not blanking: an empty value means zero devices to HIP.
		Expect(out).NotTo(ContainSubstring("CUDA_VISIBLE_DEVICES="))
	})

	It("keeps CUDA_VISIBLE_DEVICES and removes an inherited ROCr value on nvidia", func() {
		tmp := oneNodeWithGPUs()
		writeCfg(tmp, "launcher: local\n")

		sess := runSrunEnv(tmp, append(append([]string{}, dryEnv...), "ROCR_VISIBLE_DEVICES=6,7"),
			"-n", "2", "--gpus-per-task=1", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := errOf(sess)

		Expect(rankBlock(out, 0, "node001")).To(haveEnv("CUDA_VISIBLE_DEVICES=0"))
		Expect(out).NotTo(ContainSubstring("ROCR_VISIBLE_DEVICES=6,7"))
	})

	It("leaves an inherited value alone when the step holds no devices", func() {
		// Nothing is written, so nothing can layer on top; the user's own value is
		// their business. Removing it here would be the shim overreaching.
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true},
		}, []int{2})
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: amd\n")

		sess := runSrunEnv(tmp, append(append([]string{}, dryEnv...), "CUDA_VISIBLE_DEVICES=6,7"),
			"-n", "2", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(errOf(sess)).To(ContainSubstring("CUDA_VISIBLE_DEVICES=6,7"))
	})

	It("refuses a GPU step under an unknown vendor instead of guessing nvidia", func() {
		tmp := oneNodeWithGPUs()
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: intel\n")

		sess := runSrunEnv(tmp, dryEnv, "-n", "2", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(1))
		Expect(errOf(sess)).To(ContainSubstring("intel"))
	})

	It("still runs a CPU-only step under an unknown vendor", func() {
		// The refusal is scoped to steps that actually publish devices: a bad key
		// must not take down every submission on the cluster.
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true},
		}, []int{2})
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: intel\n")

		sess := runSrunEnv(tmp, dryEnv, "-n", "2", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
	})
	It("carries a UUID device id end to end without turning it into an index", func() {
		// The regression phase 2 exists for. The id used to be coerced to an int:
		// a UUID matched no numeric form, so it silently became its position in
		// the grant, and a job granted two specific devices was handed "the first
		// two devices on the node" instead.
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true,
				GPUs: []string{"GPU-0123456789abcdef", "GPU-dead0000000000ff"}},
		}, []int{2})
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: amd\n")

		sess := runSrunEnv(tmp, dryEnv, "-n", "2", "--gpus-per-task=1", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := errOf(sess)

		Expect(rankBlock(out, 0, "node001")).To(haveEnv("ROCR_VISIBLE_DEVICES=GPU-0123456789abcdef"))
		Expect(rankBlock(out, 1, "node001")).To(haveEnv("ROCR_VISIBLE_DEVICES=GPU-dead0000000000ff"))
		Expect(out).NotTo(ContainSubstring("ROCR_VISIBLE_DEVICES=0"))
	})

	It("still emits plain indices for a numeric grant", func() {
		// Widening the type must be invisible to every existing site.
		tmp := oneNodeWithGPUs()
		writeCfg(tmp, "launcher: local\n")

		sess := runSrunEnv(tmp, dryEnv, "-n", "2", "--gpus-per-task=1", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := errOf(sess)
		Expect(rankBlock(out, 0, "node001")).To(haveEnv("CUDA_VISIBLE_DEVICES=0"))
		Expect(rankBlock(out, 1, "node001")).To(haveEnv("CUDA_VISIBLE_DEVICES=1"))
	})
	// Every other spec here is a dry run, which calls RankOverlay in-process and
	// never encodes or decodes a StepSpec. Mutation testing proved that gap real:
	// changing StepSpec.GPUEnvVar's json tag to "-" -- so the vendor never crosses
	// the control channel and every real AMD rank silently falls back to
	// CUDA_VISIBLE_DEVICES -- left the whole suite green. This spec launches for
	// real, so the wire, the stepper and the removal are all in the loop.
	It("gives a real rank the ROCr mask and nothing else", func() {
		tmp := oneNodeWithGPUs()
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: amd\n")

		sess := runSrunEnv(tmp, inherited, "-n", "2", "--gpus-per-task=1",
			"sh", "-c", `echo "R $SLURM_LOCALID rocr=[${ROCR_VISIBLE_DEVICES-UNSET}] `+
				`cuda=[${CUDA_VISIBLE_DEVICES-UNSET}] hip=[${HIP_VISIBLE_DEVICES-UNSET}] `+
				`ord=[${GPU_DEVICE_ORDINAL-UNSET}]"`)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := string(sess.Out.Contents())

		// Whole lines, so a rank that saw the entire grant cannot pass as one that
		// saw a single device: "rocr=[0]" would otherwise prefix-match "rocr=[0,1]".
		Expect(out).To(ContainSubstring("R 0 rocr=[0] cuda=[UNSET] hip=[UNSET] ord=[UNSET]"))
		Expect(out).To(ContainSubstring("R 1 rocr=[1] cuda=[UNSET] hip=[UNSET] ord=[UNSET]"))
	})

	It("gives a real rank on an nvidia site the CUDA mask and nothing else", func() {
		tmp := oneNodeWithGPUs()
		writeCfg(tmp, "launcher: local\n")

		sess := runSrunEnv(tmp, inherited, "-n", "2", "--gpus-per-task=1",
			"sh", "-c", `echo "R $SLURM_LOCALID cuda=[${CUDA_VISIBLE_DEVICES-UNSET}] `+
				`rocr=[${ROCR_VISIBLE_DEVICES-UNSET}]"`)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := string(sess.Out.Contents())
		Expect(out).To(ContainSubstring("R 0 cuda=[0] rocr=[UNSET]"))
		Expect(out).To(ContainSubstring("R 1 cuda=[1] rocr=[UNSET]"))
	})

	It("carries a UUID device id to a real rank", func() {
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true,
				GPUs: []string{"GPU-0123456789abcdef", "GPU-dead0000000000ff"}},
		}, []int{2})
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: amd\n")

		sess := runSrunEnv(tmp, inherited, "-n", "2", "--gpus-per-task=1",
			"sh", "-c", `echo "R $SLURM_LOCALID rocr=[${ROCR_VISIBLE_DEVICES-UNSET}]"`)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := string(sess.Out.Contents())
		Expect(out).To(ContainSubstring("R 0 rocr=[GPU-0123456789abcdef]"))
		Expect(out).To(ContainSubstring("R 1 rocr=[GPU-dead0000000000ff]"))
	})

	It("does not leave a device-less rank holding an inherited mask", func() {
		// The overlay adds nothing to a rank with no devices, so if the shim only
		// removed the OTHER vendor's variables this rank would run with the job's
		// inherited mask, naming devices it was never granted.
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 2, IsMaster: true, GPUs: []string{"0", "1"}},
			{Index: 1, Host: "node002", Slots: 2},
		}, []int{1, 1})
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: amd\n")

		sess := runSrunEnv(tmp, inherited, "-N", "2", "-n", "2",
			"sh", "-c", `echo "R $SLURM_NODEID rocr=[${ROCR_VISIBLE_DEVICES-UNSET}] `+
				`cuda=[${CUDA_VISIBLE_DEVICES-UNSET}]"`)
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := string(sess.Out.Contents())
		Expect(out).To(ContainSubstring("R 0 rocr=[0,1] cuda=[UNSET]"))
		Expect(out).To(ContainSubstring("R 1 rocr=[UNSET] cuda=[UNSET]"))
	})
	It("warns when a GPU was requested but no devices were found", func() {
		// The usual first-install cause is gpu.gres_complex naming a complex the
		// cluster does not have. Without a warning the step runs on whatever mask
		// the job environment carried, against the node's full device list.
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true},
		}, []int{2})
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: amd\n  gres_complex: nosuchcomplex\n")

		// --gpus-per-task already errors loudly when the grant is empty. The silent
		// path is a job-level request whose devices never arrived, which is what a
		// wrong complex name produces: srun then just runs.
		sess := runSrunEnv(tmp, append(append([]string{}, dryEnv...), inherited...),
			"-n", "2", "--gpus=2", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := errOf(sess)
		Expect(out).To(ContainSubstring("requested GPUs but no granted devices were found"))
		Expect(out).To(ContainSubstring("nosuchcomplex"))
	})

	It("stays quiet about GPUs on a step that asked for none", func() {
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true},
		}, []int{2})
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: amd\n")
		sess := runSrunEnv(tmp, dryEnv, "-n", "2", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(errOf(sess)).NotTo(ContainSubstring("requested GPUs but no granted devices"))
	})

	It("warns rather than silently discarding an explicitly exported device variable", func() {
		// SLURM honours --export, so the user's most direct statement of intent
		// must not vanish without a word.
		tmp := oneNodeWithGPUs()
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: amd\n")
		sess := runSrunEnv(tmp, dryEnv, "--export=ALL,CUDA_VISIBLE_DEVICES=9",
			"-n", "2", "--gpus-per-task=1", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		out := errOf(sess)
		Expect(out).To(ContainSubstring("--export names CUDA_VISIBLE_DEVICES"))
		Expect(out).To(ContainSubstring("ROCR_VISIBLE_DEVICES"))
	})

	It("accepts an upper-case vendor rather than refusing every GPU step", func() {
		tmp := oneNodeWithGPUs()
		writeCfg(tmp, "launcher: local\ngpu:\n  vendor: AMD\n")
		sess := runSrunEnv(tmp, dryEnv, "-n", "2", "--gpus-per-task=1", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(rankBlock(errOf(sess), 0, "node001")).To(haveEnv("ROCR_VISIBLE_DEVICES=0"))
	})
	It("prints an inherited ROCr value when nothing removes it", func() {
		// Positive control. Without this, every "the inherited ROCr value is gone"
		// assertion would also pass if the report simply never printed that
		// variable -- which is exactly what a deleted filter clause would do.
		tmp := writeAlloc([]layout.Node{
			{Index: 0, Host: "node001", Slots: 4, IsMaster: true},
		}, []int{2})
		writeCfg(tmp, "launcher: local\n")
		sess := runSrunEnv(tmp, append(append([]string{}, dryEnv...),
			"ROCR_VISIBLE_DEVICES=6,7"), "-n", "2", "hostname")
		Eventually(sess, "30s").Should(gexec.Exit(0))
		Expect(errOf(sess)).To(ContainSubstring("ROCR_VISIBLE_DEVICES=6,7"))
	})
})
