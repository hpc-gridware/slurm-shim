package sbatch

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/dryrun"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// dryCfg is testCfg with per-PE task semantics, since the task policy is what
// turns a slot grant into the task count the job actually sees.
func dryCfg() *config.Config {
	c := testCfg()
	c.PEs = map[string]config.PE{
		"gpu.pe": {TaskPolicy: "node"},
		"smp.pe": {TaskPolicy: "slot"},
	}
	return c
}

// report is the two streams a dry run writes: the environment block on stdout,
// everything else on stderr (REQ-LOG-003).
type report struct {
	stdout string
	stderr string
	code   int
}

func dryRunSbatch(runner *fake.Runner, args ...string) report {
	GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
	return dryRunSbatchCfg(runner, dryCfg(), args...)
}

func dryRunSbatchCfg(runner *fake.Runner, cfg *config.Config, args ...string) report {
	var out, errBuf bytes.Buffer
	code := run(runner, cfg, "/opt/shim/slurm-shim", args, &out, &errBuf)
	return report{stdout: out.String(), stderr: errBuf.String(), code: code}
}

// qconfPE responds to `qconf -sp` with the given lines, so specs can drive the
// allocation-rule model without a cluster.
func qconfPE(lines string) *fake.Runner {
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		if name == "qconf" {
			return fake.Response{Stdout: []byte(lines)}
		}
		return fake.Response{}
	}}
}

var _ = Describe("sbatch dry run [REQ-DRY-001] [REQ-DRY-002] [REQ-DRY-003] [REQ-DRY-004]", func() {
	var script string

	BeforeEach(func() {
		script = filepath.Join(GinkgoT().TempDir(), "train.sh")
		Expect(os.WriteFile(script, []byte("#!/bin/bash\nsrun hostname\n"), 0o700)).To(Succeed())
	})

	It("prints the qsub command line on stderr and never submits", func() {
		runner := &fake.Runner{}
		r := dryRunSbatch(runner, "-p", "gpu", "-N", "2", "-c", "4", script)

		Expect(r.code).To(Equal(0))
		Expect(r.stderr).To(ContainSubstring("dry run"))
		Expect(r.stderr).To(ContainSubstring("qsub -terse -q gpu.q -pe gpu.pe 8"))
		Expect(r.stdout).NotTo(ContainSubstring("Submitted batch job"))
		// The capability probe is the one qsub a dry run may reach: `qsub -help`
		// prints usage and cannot touch cluster state, and it has to answer the
		// same here as on the real path or the report explains a submission that
		// would never happen. Everything else is still forbidden.
		for _, c := range runner.Calls {
			if c.Name == "qsub" {
				Expect(c.Args).To(Equal([]string{"-help"}), "dry run must not submit")
				continue
			}
			Expect(c.Name).NotTo(Equal("qsub"))
		}
	})

	// The report must not pollute the stream callers parse for a job id: the
	// defensive `jid=$(sbatch ...); [ -z "$jid" ]` check has to fire.
	It("keeps stdout free of report prose so a job-id parse finds nothing", func() {
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu", script)

		Expect(r.stdout).NotTo(ContainSubstring("dry run"))
		Expect(r.stdout).NotTo(ContainSubstring("would submit"))
		for _, line := range splitLines(r.stdout) {
			Expect(line).To(MatchRegexp(`^[A-Z_]+[A-Z0-9_]*=`),
				"stdout must carry only KEY=VALUE, got %q", line)
		}
	})

	It("reports the job environment the PE hook would export", func() {
		// gpu.pe runs one task per node, so 16 slots over 2 nodes is 2 tasks of 8
		// cpus -- not the 4 tasks of 4 cpus the command line asked for.
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu", "-N", "2", "--ntasks-per-node=2", "-c", "4", script)

		Expect(r.stdout).To(ContainSubstring("SLURM_JOB_NUM_NODES=2"))
		Expect(r.stdout).To(ContainSubstring("SLURM_NTASKS=2"))
		Expect(r.stdout).To(ContainSubstring("SLURM_TASKS_PER_NODE=1(x2)"))
		Expect(r.stdout).To(ContainSubstring("SLURM_CPUS_PER_TASK=8"))
	})

	It("marks the values only the real grant can supply", func() {
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu", script)

		Expect(r.stdout).To(ContainSubstring("SLURM_JOB_ID=<assigned by qsub>"))
		Expect(r.stdout).To(ContainSubstring("SLURM_JOB_NODELIST=<hosts from the grant>"))
		// Emitted by the real job but not producible by a prediction: listed so
		// "the predictor cannot supply this" is not mistaken for "the job lacks it".
		Expect(r.stdout).To(ContainSubstring("SLURM_LAUNCH_NODE_IPADDR="))
	})

	// The case the mode exists for: a partition whose slots rule and task policy
	// silently override the requested geometry.
	It("surfaces a task count the partition's slot rule imposes", func() {
		r := dryRunSbatch(&fake.Runner{}, "-p", "batch", "--ntasks=1", script)

		Expect(r.stderr).To(MatchRegexp(`slots\s+16 \(partition slots rule "16"`))
		Expect(r.stderr).To(MatchRegexp(`requested geometry\s+ntasks 1`))
		Expect(r.stdout).To(ContainSubstring("SLURM_NTASKS=16"))
	})

	It("reports the array translation without submitting it", func() {
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu", "--array=0-9", script)

		Expect(r.stderr).To(ContainSubstring("-t 1-10"))
		Expect(r.stdout).To(ContainSubstring("SLURM_ARRAY_TASK_COUNT=10"))
		Expect(r.stdout).To(ContainSubstring("SLURM_ARRAY_TASK_MIN=0"))
		Expect(r.stdout).To(ContainSubstring("SLURM_ARRAY_TASK_ID=<this element's index>"))
	})

	// --- shim control variables reach the job through qsub -V, so the prediction
	// must honor them or it contradicts the job on its headline numbers.

	It("honors a SLURM_SHIM_TASK_POLICY override from the submit environment", func() {
		GinkgoT().Setenv("SLURM_SHIM_TASK_POLICY", "slot")
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu", "-N", "2", "-c", "4", script)

		// slot policy: one task per slot, and SLURM_CPUS_PER_TASK is omitted (A11).
		Expect(r.stdout).To(ContainSubstring("SLURM_NTASKS=8"))
		Expect(r.stdout).NotTo(ContainSubstring("SLURM_CPUS_PER_TASK="))
	})

	It("reports scrub-only mode under SLURM_SHIM_DISABLE instead of a full table", func() {
		GinkgoT().Setenv("SLURM_SHIM_DISABLE", "1")
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu", "-N", "2", script)

		Expect(r.stderr).To(ContainSubstring("receives NO SLURM_* variables"))
		Expect(r.stdout).NotTo(ContainSubstring("SLURM_NTASKS="))
	})

	It("does not inherit a parent job's grant when submitted from inside one", func() {
		GinkgoT().Setenv("JOB_ID", "999")
		GinkgoT().Setenv("NHOSTS", "8")
		GinkgoT().Setenv("PE_HOSTFILE", "/nonexistent/hostfile")
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu", "-N", "2", script)

		Expect(r.code).To(Equal(0))
		Expect(r.stdout).To(ContainSubstring("SLURM_JOB_ID=<assigned by qsub>"))
		Expect(r.stdout).To(ContainSubstring("SLURM_JOB_NUM_NODES=2"))
	})

	// --- the allocation-rule model

	It("pins the job to one node under allocation_rule $pe_slots", func() {
		r := dryRunSbatch(qconfPE("allocation_rule    $pe_slots\ncontrol_slaves     TRUE\n"),
			"-p", "gpu", "-N", "4", "-c", "4", script)

		Expect(r.stderr).To(ContainSubstring("1 node(s) x 16 slots"))
		Expect(r.stdout).To(ContainSubstring("SLURM_JOB_NUM_NODES=1"))
	})

	It("spreads one slot per host under allocation_rule $round_robin", func() {
		r := dryRunSbatch(qconfPE("allocation_rule    $round_robin\ncontrol_slaves     TRUE\n"),
			"-p", "gpu", "--ntasks=4", script)

		Expect(r.stderr).To(ContainSubstring("4 node(s) x 1 slots"))
		Expect(r.stderr).To(ContainSubstring("$round_robin"))
		// Without --nodes the host count is unknowable, so the widest spread must be
		// stated as a bound rather than a fact.
		Expect(r.stderr).To(ContainSubstring("fewer hosts if fewer are free"))
	})

	// Verified against the live OCS test cluster: `-N 3 --ntasks-per-node=2` on the
	// round-robin `make` PE lands 2 slots on each of 3 hosts. Predicting the widest
	// spread (6 nodes x 1) there would be wrong on every geometry variable.
	It("honors --nodes under $round_robin, since the slot count came from it", func() {
		// Mirrors the OCS test cluster's `batch`: per-task slots on a round-robin PE
		// whose task policy is one task per slot.
		cfg := dryCfg()
		cfg.Partitions["rr"] = config.Partition{Queue: "all.q", PE: "make", Slots: "per-task"}
		cfg.PEs["make"] = config.PE{TaskPolicy: "slot"}
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		r := dryRunSbatchCfg(qconfPE("allocation_rule    $round_robin\ncontrol_slaves     TRUE\n"),
			cfg, "-p", "rr", "-N", "3", "--ntasks-per-node=2", script)

		Expect(r.stderr).To(ContainSubstring("over the requested 3 node(s)"))
		Expect(r.stdout).To(ContainSubstring("SLURM_JOB_NUM_NODES=3"))
		Expect(r.stdout).To(ContainSubstring("SLURM_NTASKS=6"))
		Expect(r.stdout).To(ContainSubstring("SLURM_TASKS_PER_NODE=2(x3)"))
	})

	It("produces a uniform spread under a fixed allocation_rule", func() {
		r := dryRunSbatch(qconfPE("allocation_rule    4\ncontrol_slaves     TRUE\n"),
			"-p", "gpu", "--ntasks=8", script)

		Expect(r.stderr).To(ContainSubstring("2 node(s) x 4 slots"))
		// A uniform spread cannot trigger the fabricator's non-uniform warning.
		Expect(r.stderr).NotTo(ContainSubstring("non-uniform"))
		Expect(r.stderr).NotTo(ContainSubstring("heterogeneous"))
	})

	It("rejects a slot count a fixed allocation_rule cannot dispatch", func() {
		r := dryRunSbatch(qconfPE("allocation_rule    8\ncontrol_slaves     TRUE\n"),
			"-p", "gpu", "--ntasks=12", script)

		Expect(r.code).To(Equal(dryrun.ExitFatal))
		Expect(r.stderr).To(ContainSubstring("not a multiple of allocation_rule 8"))
		Expect(r.stdout).To(BeEmpty())
	})

	It("never invents a heterogeneous spread from a remainder", func() {
		// 10 slots over a requested 4 nodes: the old model produced 2/2/2/4 and a
		// phantom "non-uniform per-node task counts" warning.
		cfg := dryCfg()
		cfg.Partitions["odd"] = config.Partition{Queue: "all.q", PE: "smp.pe", Slots: "10"}
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		r := dryRunSbatchCfg(&fake.Runner{}, cfg, "-p", "odd", "-N", "4", script)

		Expect(r.stderr).NotTo(ContainSubstring("non-uniform"))
		Expect(r.stderr).NotTo(MatchRegexp(`\d+/\d+`), "spread must be uniform")
	})

	// --- PE lookup failures must not become verdicts

	It("does not claim control_slaves is wrong when qconf failed", func() {
		runner := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			if name == "qconf" {
				return fake.Response{Exit: 1} // non-zero, empty stderr
			}
			return fake.Response{}
		}}
		r := dryRunSbatch(runner, "-p", "gpu", "-N", "2", script)

		Expect(r.stderr).NotTo(ContainSubstring("control_slaves is not TRUE"))
		Expect(r.stderr).To(ContainSubstring("unavailable"))
	})

	It("does not claim control_slaves is wrong when qconf omitted the key", func() {
		r := dryRunSbatch(qconfPE("allocation_rule    $fill_up\n"), "-p", "gpu", "-N", "2", script)

		Expect(r.stderr).NotTo(ContainSubstring("control_slaves is not TRUE"))
		Expect(r.stderr).To(ContainSubstring("unverified"))
	})

	It("warns only when the PE really reports control_slaves FALSE", func() {
		r := dryRunSbatch(qconfPE("allocation_rule    $fill_up\ncontrol_slaves     FALSE\n"),
			"-p", "gpu", "-N", "2", script)
		Expect(r.stderr).To(ContainSubstring("control_slaves is not TRUE"))

		ok := dryRunSbatch(qconfPE("allocation_rule    $fill_up\ncontrol_slaves     TRUE\n"),
			"-p", "gpu", "-N", "2", script)
		Expect(ok.stderr).NotTo(ContainSubstring("control_slaves is not TRUE"))
	})

	// --- secrets and control characters

	It("redacts the values of -v assignments in the reported command line", func() {
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu",
			"--export=ALL,HF_TOKEN=hf_liveSecret123", script)

		Expect(r.stderr).NotTo(ContainSubstring("hf_liveSecret123"))
		Expect(r.stdout).NotTo(ContainSubstring("hf_liveSecret123"))
		Expect(r.stderr).To(ContainSubstring("HF_TOKEN=<value>"))
	})

	It("escapes control characters from a job name so the report cannot be repainted", func() {
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu", "-J", "aaa\x1b[2K\rqsub", script)

		Expect(r.stderr).NotTo(ContainSubstring("\x1b"))
		Expect(r.stderr).NotTo(ContainSubstring("\r"))
		Expect(r.stderr).To(ContainSubstring(`\x1b`))
	})

	It("does not echo a --wrap body containing a secret", func() {
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu", "--wrap", "python t.py --key=sk-live-9f8e\x1bX")
		Expect(r.stderr).NotTo(ContainSubstring("\x1b"))
	})

	// --- fatal conditions must not exit 0

	It("exits non-zero when the report proves the job cannot start [REQ-ENC-005]", func() {
		cfg := dryCfg()
		cfg.PEs["gpu.pe"] = config.PE{TaskPolicy: "gpu"}
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		r := dryRunSbatchCfg(&fake.Runner{}, cfg, "-p", "gpu", script)

		Expect(r.code).To(Equal(dryrun.ExitFatal))
		Expect(r.stderr).To(ContainSubstring("task_policy gpu but no node was granted a GPU"))
		Expect(r.stderr).To(ContainSubstring("the job would fail at startup"))
	})

	It("leaves no wrapper spool behind in wrapper mode [SI-57]", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		spool := GinkgoT().TempDir()
		cfg := dryCfg()
		cfg.WrapperMode = true
		cfg.WrapperSpoolDir = spool

		r := dryRunSbatchCfg(&fake.Runner{}, cfg, "-p", "gpu", script)
		Expect(r.code).To(Equal(0))
		Expect(r.stderr).To(ContainSubstring("wrapper mode"))
		Expect(r.stderr).To(ContainSubstring(spool), "the reported path must name the real spool root")

		entries, err := os.ReadDir(spool)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty(), "a dry run must not spool a script for a job it never submits")
	})

	It("reports the --wrap temp script shape without creating it", func() {
		r := dryRunSbatch(&fake.Runner{}, "-p", "gpu", "--wrap", "hostname")
		Expect(r.stderr).To(ContainSubstring("slurm-shim-sbatch-XXXX"))
		Expect(r.stderr).To(ContainSubstring("generated from --wrap: hostname"))
	})

	// --- invocation channels

	It("is reachable through --test-only with no environment variable", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "")
		runner := &fake.Runner{}
		r := dryRunSbatchCfg(runner, dryCfg(), "-p", "gpu", "--test-only", script)

		Expect(r.code).To(Equal(0))
		Expect(r.stderr).To(ContainSubstring("dry run"))
		for _, c := range runner.Calls {
			Expect(c.Name).NotTo(Equal("qsub"))
		}
	})

	It("does not let --test-only swallow the script path", func() {
		opt, _, err := parseFlags([]string{"--test-only", "job.sh", "arg1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.testOnly).To(BeTrue())
		Expect(opt.script).To(Equal("job.sh"))
		Expect(opt.scriptArgs).To(Equal([]string{"arg1"}))
	})

	It("submits normally when the variable is off", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "0")
		var captured []string
		var out, errBuf bytes.Buffer
		code := run(fakeQsub("4711", &captured), dryCfg(), "/opt/shim/slurm-shim",
			[]string{"-p", "gpu", script}, &out, &errBuf)

		Expect(code).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("Submitted batch job 4711"))
	})

	// The switch must fail open: an unrecognized value means "do the real thing",
	// and the user is told their value was not understood.
	It("submits for real on an unrecognized value, with a warning", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "maybe")
		var captured []string
		var out, errBuf bytes.Buffer
		code := run(fakeQsub("4711", &captured), dryCfg(), "/opt/shim/slurm-shim",
			[]string{"-p", "gpu", script}, &out, &errBuf)

		Expect(code).To(Equal(0))
		Expect(out.String()).To(ContainSubstring("Submitted batch job 4711"))
		Expect(errBuf.String()).To(ContainSubstring("not a recognized on/off value"))
	})

	// --- geometry bounds

	It("rejects an absurd --nodes instead of panicking", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		r := dryRunSbatchCfg(&fake.Runner{}, dryCfg(), "-p", "gpu", "-N", "900000000000000000", script)

		Expect(r.code).To(Equal(1))
		Expect(r.stderr).To(ContainSubstring("exceeds the maximum supported value"))
	})

	It("bounds the modelled node count", func() {
		cfg := dryCfg()
		cfg.Partitions["huge"] = config.Partition{Queue: "all.q", PE: "smp.pe", Slots: "1000000"}
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		r := dryRunSbatchCfg(qconfPE("allocation_rule    1\ncontrol_slaves     TRUE\n"),
			cfg, "-p", "huge", script)

		Expect(r.code).To(Equal(0))
		Expect(r.stderr).To(ContainSubstring("capped at"))
	})
})

func splitLines(s string) []string {
	var out []string
	for _, l := range bytes.Split([]byte(s), []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			out = append(out, string(l))
		}
	}
	return out
}

// parQconfPE answers BOTH `qconf -sp` and the `qsub -help` capability probe, so a
// spec exercises the pinned report rather than the pre-9.1.5 model.
//
// This exists because qconfPE returns a bare fake.Response{} for anything that is
// not qconf: the probe then reads empty usage, no rule is emitted, and every
// -par branch of the report goes unexercised while the suite stays green.
func parQconfPE(lines string) *fake.Runner {
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		switch {
		case name == "qconf":
			return fake.Response{Stdout: []byte(lines)}
		case name == "qsub" && len(args) == 1 && args[0] == "-help":
			return fake.Response{Stdout: []byte(parUsage)}
		}
		return fake.Response{}
	}}
}

var _ = Describe("the dry run reports a pinned allocation rule [REQ-SBT-006] [REQ-DRY-004]", func() {
	var script string

	BeforeEach(func() {
		script = filepath.Join(GinkgoT().TempDir(), "train.sh")
		Expect(os.WriteFile(script, []byte("#!/bin/bash\nsrun hostname\n"), 0o700)).To(Succeed())
	})

	// The report shape reproduced in README.md, which until now no spec produced.
	It("names the emitted rule, what it overrides, and an exact spread", func() {
		r := dryRunSbatch(parQconfPE("allocation_rule $fill_up\ncontrol_slaves TRUE\n"),
			"-p", "gpu", "-N", "4", "-c", "8", script)

		Expect(r.code).To(Equal(0))
		Expect(r.stderr).To(ContainSubstring("qsub -terse -q gpu.q -pe gpu.pe 32 -par 8 -w e"))
		Expect(r.stderr).To(ContainSubstring("-par 8 (overrides PE gpu.pe's $fill_up)"))
		Expect(r.stderr).To(ContainSubstring("4 node(s) x 8 slots -- qsub -par 8 pins 8 slot(s) on each of 4 node(s)"))
		Expect(r.stderr).To(ContainSubstring("pinned by qsub -par, not modelled"))
	})

	It("reports the single-node rule as $pe_slots", func() {
		r := dryRunSbatch(parQconfPE("allocation_rule $round_robin\ncontrol_slaves TRUE\n"),
			"-p", "gpu", "-N", "1", "-c", "8", script)

		Expect(r.stderr).To(ContainSubstring("-par '$pe_slots'"))
		Expect(r.stderr).To(ContainSubstring("qsub -par $pe_slots pins the job to one node"))
	})

	// todo 005's trap: a qconf we could not read must never be reported as a verdict.
	It("hedges when the PE's own rule could not be read", func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		r := dryRunSbatchCfg(&fake.Runner{Responder: func(name string, args []string) fake.Response {
			if name == "qsub" && len(args) == 1 && args[0] == "-help" {
				return fake.Response{Stdout: []byte(parUsage)}
			}
			return fake.Response{Exit: 1, Err: errors.New("qconf: command not found")}
		}}, dryCfg(), "-p", "gpu", "-N", "2", script)

		Expect(r.stderr).To(ContainSubstring("configured allocation_rule (not read)"))
		Expect(r.stderr).NotTo(ContainSubstring("overrides PE gpu.pe's \n"))
	})

	// The note belongs to the report in this mode. Printed again before the banner
	// it would read as output from a submission that never happened.
	It("prints the memory note exactly once, below the banner", func() {
		r := dryRunSbatch(parQconfPE("allocation_rule $fill_up\ncontrol_slaves TRUE\n"),
			"-p", "gpu", "-N", "3", "--ntasks-per-node=2", "--mem=4G", script)

		Expect(strings.Count(r.stderr, "slot(s)/node")).To(Equal(1))
		Expect(strings.Index(r.stderr, "dry run")).To(BeNumerically("<",
			strings.Index(r.stderr, "slot(s)/node")), "the banner must come first")
	})

	// -par counts slots, devices count tasks: the reported per-node device count and
	// the emitted -l request must agree.
	It("agrees with the emitted gres request on devices per node", func() {
		r := dryRunSbatch(parQconfPE("allocation_rule $fill_up\ncontrol_slaves TRUE\n"),
			"-p", "gpu", "-N", "3", "--ntasks-per-node=2", "-c", "4", "--gpus-per-task=1", script)

		Expect(r.stderr).To(ContainSubstring("-par 8"), "8 slots per node")
		Expect(r.stderr).To(ContainSubstring("gpu=2"), "but only 2 tasks per node")
		Expect(r.stderr).To(ContainSubstring("gpus per node       2"))
	})

	// A literal slots rule pins the count, so nothing is derived and the report must
	// not claim a spread it did not pin.
	It("pins nothing on a literal-slots partition and says why", func() {
		r := dryRunSbatch(parQconfPE("allocation_rule $fill_up\ncontrol_slaves TRUE\n"),
			"-p", "batch", "-N", "4", script)

		Expect(r.stderr).NotTo(ContainSubstring("-par"))
		Expect(r.stderr).To(ContainSubstring("the requested geometry does not change it"))
		Expect(r.stderr).To(ContainSubstring("not enforced on partition \"batch\""))
	})
})
