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
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

// parSpecFor parses a real flag line and derives the rule for a partition slot
// rule, so the table exercises the same geometry resolution a submission does.
func parSpecFor(slotsRule string, tokens ...string) (allocationRule, int) {
	GinkgoHelper()
	opt, _, err := parseFlags(append(tokens, "job.sh"))
	Expect(err).NotTo(HaveOccurred())
	opt.partition = "testpart"
	part := config.Partition{Slots: slotsRule}
	slots, err := computeSlots(opt, part)
	Expect(err).NotTo(HaveOccurred())
	return parSpec(opt, part, slots), slots
}

var _ = Describe("parSpec [REQ-SBT-006]", func() {
	DescribeTable("derives the allocation rule from a stated geometry",
		func(slotsRule string, tokens []string, wantSlots int, wantValue string, wantNodes int) {
			got, slots := parSpecFor(slotsRule, tokens...)
			Expect(slots).To(Equal(wantSlots))
			Expect(got.Value).To(Equal(wantValue))
			Expect(got.Nodes).To(Equal(wantNodes))
			Expect(got.Warn).To(BeEmpty())
			if got.emit() {
				// The invariant that keeps a job out of a permanent qw: a fixed rule
				// only dispatches when the slot count is a multiple of it.
				if got.Value != "$pe_slots" {
					Expect(slots % wantNodes).To(Equal(0))
					Expect(slots / wantNodes).To(BeNumerically(">=", 1))
				}
			}
		},
		Entry("-N with --ntasks-per-node", "per-task",
			[]string{"-N", "3", "--ntasks-per-node=2"}, 6, "2", 3),
		Entry("cpus-per-task scales slots, not tasks", "per-task",
			[]string{"-N", "3", "--ntasks-per-node=2", "-c", "4"}, 24, "8", 3),
		Entry("-N alone is one task per node", "per-task",
			[]string{"-N", "3"}, 3, "1", 3),
		Entry("a single node pins the whole grant", "per-task",
			[]string{"-N", "1", "-c", "8"}, 8, "$pe_slots", 1),
		Entry("--ntasks-per-node without -N is one node", "per-task",
			[]string{"--ntasks-per-node=4"}, 4, "$pe_slots", 1),
		Entry("node count derived from --ntasks / per-node", "per-task",
			[]string{"-n", "6", "--ntasks-per-node=2"}, 6, "2", 3),
		Entry("--ntasks over --nodes", "per-task",
			[]string{"-N", "2", "-n", "6"}, 6, "3", 2),
		Entry("no geometry stated leaves the PE in charge", "per-task",
			[]string{"-n", "16"}, 16, "", 0),
		Entry("a bare script states nothing", "per-task",
			[]string{}, 1, "", 0),
	)

	DescribeTable("declines to pin a layout Grid Engine cannot grant",
		func(slotsRule string, tokens []string) {
			got, _ := parSpecFor(slotsRule, tokens...)
			Expect(got.emit()).To(BeFalse())
			Expect(got.Value).To(BeEmpty())
			Expect(got.Warn).To(ContainSubstring("uneven"))
			Expect(got.Warn).To(ContainSubstring("allocation_rule"))
		},
		// SLURM would spread these 3,2,2 or 2,2,2,1. Grid Engine grants the same
		// count on every host under a fixed rule, so there is no faithful value --
		// and emitting one anyway would leave the job in qw forever.
		Entry("-N 3 with an indivisible task count", "per-task", []string{"-N", "3", "-n", "7"}),
		Entry("per-node count that does not divide --ntasks", "per-task",
			[]string{"-n", "7", "--ntasks-per-node=2"}),
		Entry("more nodes than tasks", "per-task", []string{"-N", "4", "-n", "2"}),
	)

	// A literal `slots:` rule is the site saying geometry does not size this
	// partition -- computeSlots discards --ntasks for it entirely. Deriving a rule
	// from that site-chosen total fabricates a per-node width nobody stated, and
	// -w e turns it into a hard refusal for a job that ran before the change.
	DescribeTable("never pins a layout on a partition whose slot count the site pinned",
		func(tokens []string, wantSlots int) {
			got, slots := parSpecFor("16", tokens...)
			Expect(slots).To(Equal(wantSlots))
			Expect(got.emit()).To(BeFalse())
			Expect(got.Warn).To(ContainSubstring("not enforced on partition"))
			Expect(got.Warn).To(ContainSubstring("16 slot(s)"))
			Expect(got.Warn).NotTo(ContainSubstring("uneven"),
				"the task count is exactly what this partition ignores")
		},
		// The regression: this emitted -par $pe_slots and was refused on hosts with
		// fewer than 16 slots, with the user's --ntasks invisible in the diagnostic.
		Entry("single node, explicit task count", []string{"-N", "1", "-n", "4"}, 16),
		Entry("node count that divides the fixed total", []string{"-N", "4"}, 16),
		Entry("node count that does not divide it", []string{"-N", "3"}, 16),
		Entry("tasks per node", []string{"--ntasks-per-node=2"}, 16),
	)

	It("stays silent on a literal-slots partition when no layout was stated", func() {
		got, _ := parSpecFor("16", "-n", "8")
		Expect(got.emit()).To(BeFalse())
		Expect(got.Warn).To(BeEmpty(), "nothing was asked for, so nothing was denied")
	})

	// validateGeometry accepts zero (it rejects only negatives and absurd values),
	// so these reach the derivation and would divide by zero -- the crash class
	// todo 014 closed for --nodes.
	DescribeTable("treats a zero geometry as unstated rather than dividing by it",
		func(tokens []string) {
			Expect(func() {
				got, _ := parSpecFor("per-task", tokens...)
				Expect(got.emit()).To(BeFalse())
				Expect(got.Warn).To(BeEmpty())
			}).NotTo(Panic())
		},
		Entry("--nodes=0", []string{"-N", "0"}),
		Entry("--ntasks-per-node=0", []string{"--ntasks-per-node=0"}),
		Entry("both zero", []string{"-N", "0", "--ntasks-per-node=0"}),
		Entry("zero per-node with a task count", []string{"-n", "4", "--ntasks-per-node=0"}),
	)

	It("stays bounded at the maximum supported geometry", func() {
		// todo 014: an unvalidated --nodes drove an unbounded allocation. The
		// derivation must not reintroduce a cost proportional to the value.
		got, slots := parSpecFor("per-task", "-N", "1048576")
		Expect(slots).To(Equal(1048576))
		Expect(got.Value).To(Equal("1"))
		Expect(got.Nodes).To(Equal(1048576))
	})

	It("names the real numbers in the uneven-layout warning", func() {
		got, _ := parSpecFor("per-task", "-N", "3", "-n", "7")
		Expect(got.Warn).To(ContainSubstring("7 task(s) over 3 node(s)"))
	})
})

// parUsage is the one line the capability probe looks for. The full-fixture
// parsing is covered in internal/gedata; these specs care about the wiring.
const parUsage = "OCS 9.1.5 (250826-0734)\nusage: qsub [options]\n" +
	"   [-par allocation_rule]                   set the parallel job allocation rule\n"

// parRunner answers the capability probe and captures the submission argv.
// scTable is the `qconf -sc` layout the memory note consults: mem_free is not
// consumable (the stock OCS value), m_mem_free is per-slot.
const scTable = "#name shortcut type relop requestable consumable default urgency\n" +
	"mem_free            mf         MEMORY      <=    YES         NO         0        0\n" +
	"m_mem_free          mfr        MEMORY      <=    YES         YES        0        0\n"

func parRunner(usage string, capture *[]string) *fake.Runner {
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		if name == "qconf" {
			return fake.Response{Stdout: []byte(scTable)}
		}
		Expect(name).To(Equal("qsub"))
		if len(args) == 1 && args[0] == "-help" {
			return fake.Response{Stdout: []byte(usage)}
		}
		*capture = append([]string{}, args...)
		return fake.Response{Stdout: []byte("4711\n")}
	}}
}

func submitWith(cfg *config.Config, usage string, args ...string) ([]string, string, int) {
	var captured []string
	script := filepath.Join(GinkgoT().TempDir(), "job.sh")
	Expect(os.WriteFile(script, []byte("#!/bin/bash\nsrun hostname\n"), 0o700)).To(Succeed())
	var out, errOut bytes.Buffer
	code := run(parRunner(usage, &captured), cfg, "/usr/bin/slurm-shim",
		append(args, script), &out, &errOut)
	return captured, errOut.String(), code
}

var _ = Describe("sbatch emits the allocation rule [REQ-SBT-006]", func() {
	var cfg *config.Config
	BeforeEach(func() { cfg = testCfg() })

	It("emits -par with -w e for a stated geometry", func() {
		argv, _, code := submitWith(cfg, parUsage, "-p", "gpu", "-N", "3", "--ntasks-per-node=2")
		Expect(code).To(Equal(0))
		Expect(argv).To(ContainElements("-pe", "gpu.pe", "6", "-par", "2", "-w", "e"))
	})

	It("pins a single-node request with $pe_slots", func() {
		argv, _, _ := submitWith(cfg, parUsage, "-p", "gpu", "-N", "1", "-c", "8")
		Expect(argv).To(ContainElements("-par", "$pe_slots"))
	})

	// -par counts slots; devices are counted per TASK. Deriving the gres request
	// from the rule would ask for 8 GPUs where the job runs 2 tasks per node.
	It("scales --gpus-per-task by tasks per node, not by the -par slot count", func() {
		argv, _, _ := submitWith(cfg, parUsage,
			"-p", "gpu", "-N", "3", "--ntasks-per-node=2", "-c", "4", "--gpus-per-task=1")
		Expect(argv).To(ContainElements("-par", "8"), "8 slots per node")
		Expect(argv).To(ContainElement("gpu=2"), "but only 2 tasks per node")
	})

	It("emits neither flag when no geometry was stated", func() {
		argv, errOut, _ := submitWith(cfg, parUsage, "-p", "gpu", "-n", "16")
		Expect(argv).NotTo(ContainElement("-par"))
		Expect(argv).NotTo(ContainElement("-w"))
		Expect(errOut).To(BeEmpty(), "a request that stated no layout gets no new output")
	})

	It("warns and emits nothing for a layout Grid Engine cannot grant", func() {
		argv, errOut, code := submitWith(cfg, parUsage, "-p", "gpu", "-N", "3", "-n", "7")
		Expect(code).To(Equal(0))
		Expect(argv).NotTo(ContainElement("-par"))
		Expect(errOut).To(ContainSubstring("uneven"))
	})

	It("emits nothing on a cluster whose qsub has no -par, and says why", func() {
		argv, errOut, code := submitWith(cfg, "usage: qsub [options]\n   [-pe pe-name slot_range]\n",
			"-p", "gpu", "-N", "3", "--ntasks-per-node=2")
		Expect(code).To(Equal(0))
		Expect(argv).NotTo(ContainElement("-par"))
		Expect(errOut).To(ContainSubstring("this cluster's qsub has no -par"))
		Expect(errOut).To(ContainSubstring("9.1.5"))
	})

	It("honors a per-partition opt-out quietly", func() {
		p := cfg.Partitions["gpu"]
		p.AllocationRuleOverride = config.OverrideNever
		cfg.Partitions["gpu"] = p

		argv, errOut, _ := submitWith(cfg, parUsage, "-p", "gpu", "-N", "3", "--ntasks-per-node=2")
		Expect(argv).NotTo(ContainElement("-par"))
		Expect(errOut).To(BeEmpty(), "an explicit opt-out is not a warning")
	})

	It("lets a partition opt out of a site-wide always", func() {
		cfg.AllocationRuleOverride = config.OverrideAlways
		p := cfg.Partitions["gpu"]
		p.AllocationRuleOverride = config.OverrideNever
		cfg.Partitions["gpu"] = p

		argv, _, _ := submitWith(cfg, parUsage, "-p", "gpu", "-N", "2")
		Expect(argv).NotTo(ContainElement("-par"))
	})

	It("skips the probe entirely under always", func() {
		cfg.AllocationRuleOverride = config.OverrideAlways
		var captured []string
		probed := false
		r := &fake.Runner{Responder: func(name string, args []string) fake.Response {
			if len(args) == 1 && args[0] == "-help" {
				probed = true
				return fake.Response{}
			}
			captured = append([]string{}, args...)
			return fake.Response{Stdout: []byte("4711\n")}
		}}
		script := filepath.Join(GinkgoT().TempDir(), "job.sh")
		Expect(os.WriteFile(script, []byte("#!/bin/bash\n"), 0o700)).To(Succeed())
		var out, errOut bytes.Buffer
		Expect(run(r, cfg, "/usr/bin/slurm-shim",
			[]string{"-p", "gpu", "-N", "2", script}, &out, &errOut)).To(Equal(0))

		Expect(probed).To(BeFalse())
		Expect(captured).To(ContainElements("-par", "1"))
	})

	It("does not probe when no rule would be emitted", func() {
		probed := false
		r := &fake.Runner{Responder: func(_ string, args []string) fake.Response {
			if len(args) == 1 && args[0] == "-help" {
				probed = true
			}
			return fake.Response{Stdout: []byte("4711\n")}
		}}
		script := filepath.Join(GinkgoT().TempDir(), "job.sh")
		Expect(os.WriteFile(script, []byte("#!/bin/bash\n"), 0o700)).To(Succeed())
		var out, errOut bytes.Buffer
		Expect(run(r, cfg, "/usr/bin/slurm-shim",
			[]string{"-p", "gpu", "-n", "4", script}, &out, &errOut)).To(Equal(0))

		Expect(probed).To(BeFalse(), "a request with no layout must cost no extra process")
	})

	It("reports a probe failure as a probe failure, not an old cluster", func() {
		r := &fake.Runner{Responder: func(_ string, args []string) fake.Response {
			if len(args) == 1 && args[0] == "-help" {
				return fake.Response{Err: errors.New("executable file not found in $PATH")}
			}
			return fake.Response{Stdout: []byte("4711\n")}
		}}
		script := filepath.Join(GinkgoT().TempDir(), "job.sh")
		Expect(os.WriteFile(script, []byte("#!/bin/bash\n"), 0o700)).To(Succeed())
		var out, errOut bytes.Buffer
		Expect(run(r, cfg, "/usr/bin/slurm-shim",
			[]string{"-p", "gpu", "-N", "3", script}, &out, &errOut)).To(Equal(0))

		Expect(errOut.String()).To(ContainSubstring("could not probe qsub"))
		Expect(errOut.String()).NotTo(ContainSubstring("has no -par"))
	})

	It("states the per-node memory without multiplying a non-consumable complex", func() {
		// mem_free is consumable NO, so GE does no per-slot multiplication and the
		// note must not imply one (todos/045).
		_, errOut, _ := submitWith(cfg, parUsage,
			"-p", "gpu", "-N", "3", "--ntasks-per-node=2", "--mem=4G")
		Expect(errOut).To(ContainSubstring("mem_free=4G per node"))
		Expect(errOut).To(ContainSubstring("consumable NO"))
		Expect(errOut).NotTo(ContainSubstring("x 2 slot(s)/node"))
	})

	It("multiplies by slots for a per-slot consumable complex", func() {
		perSlot := *cfg
		perSlot.MemoryComplex = "m_mem_free"
		_, errOut, _ := submitWith(&perSlot, parUsage,
			"-p", "gpu", "-N", "3", "--ntasks-per-node=2", "--mem=4G")
		Expect(errOut).To(ContainSubstring("m_mem_free=4G x 2 slot(s)/node"))
		Expect(errOut).To(ContainSubstring("per-slot consumable"))
	})
})

var _ = Describe("consistency warnings a pinned layout makes possible", func() {
	var cfg *config.Config
	BeforeEach(func() {
		cfg = testCfg()
		cfg.PEs = map[string]config.PE{"gpu.pe": {TaskPolicy: "slot"}}
	})

	It("says which of a contradictory triple actually won", func() {
		_, errOut, _ := submitWith(cfg, parUsage,
			"-p", "gpu", "-N", "2", "-n", "8", "--ntasks-per-node=2")
		Expect(errOut).To(ContainSubstring("contradicts"))
		Expect(errOut).To(ContainSubstring("4 task(s) per node"))
	})

	It("stays quiet when the triple is consistent", func() {
		_, errOut, _ := submitWith(cfg, parUsage,
			"-p", "gpu", "-N", "2", "-n", "4", "--ntasks-per-node=2")
		Expect(errOut).NotTo(ContainSubstring("contradicts"))
	})

	// The spread is exact, but SLURM_NTASKS still comes from the task policy --
	// pinning placement does not make the task count match what was asked for.
	It("warns when a node task policy will not yield the requested --ntasks", func() {
		cfg.PEs = map[string]config.PE{"gpu.pe": {TaskPolicy: "node"}}
		_, errOut, _ := submitWith(cfg, parUsage, "-p", "gpu", "-N", "3", "-n", "6")
		Expect(errOut).To(ContainSubstring("task_policy node"))
		Expect(errOut).To(ContainSubstring("SLURM_NTASKS=3"))
	})

	It("does not warn about task policy under a slot policy", func() {
		_, errOut, _ := submitWith(cfg, parUsage, "-p", "gpu", "-N", "3", "-n", "6")
		Expect(errOut).NotTo(ContainSubstring("task_policy"))
	})

	It("emits no consistency warning when no rule was pinned", func() {
		cfg.PEs = map[string]config.PE{"gpu.pe": {TaskPolicy: "node"}}
		_, errOut, _ := submitWith(cfg, parUsage, "-p", "gpu", "-n", "6")
		Expect(errOut).To(BeEmpty())
	})
})

// failingQsub answers the capability probe, then fails the submission with a
// given stderr so the error-mapping branches can be exercised without a cluster.
func failingQsub(usage, geStderr string) *fake.Runner {
	return &fake.Runner{Responder: func(_ string, args []string) fake.Response {
		if len(args) == 1 && args[0] == "-help" {
			return fake.Response{Stdout: []byte(usage)}
		}
		return fake.Response{Stderr: []byte(geStderr), Exit: 1}
	}}
}

func submitFailing(cfg *config.Config, geStderr string, args ...string) (string, int) {
	script := filepath.Join(GinkgoT().TempDir(), "job.sh")
	Expect(os.WriteFile(script, []byte("#!/bin/bash\n"), 0o700)).To(Succeed())
	var out, errOut bytes.Buffer
	code := run(failingQsub(parUsage, geStderr), cfg, "/usr/bin/slurm-shim",
		append(args, script), &out, &errOut)
	return errOut.String(), code
}

var _ = Describe("a qsub refusal is only translated when it is one [REQ-SBT-006]", func() {
	var cfg *config.Config
	BeforeEach(func() { cfg = testCfg() })

	// The live shape: `qsub -w e` refusing an unschedulable layout.
	const refusal = "Unable to run job: error: no suitable queues.\nExiting.\n"

	It("renders a -w e refusal in SLURM's shape", func() {
		errOut, code := submitFailing(cfg, refusal, "-p", "gpu", "-N", "3", "--ntasks-per-node=2")

		Expect(code).To(Equal(1))
		Expect(errOut).To(ContainSubstring("Requested node configuration is not available"))
		Expect(errOut).To(ContainSubstring("3 node(s) with 2 slot(s) each"))
		Expect(errOut).To(ContainSubstring("queue gpu.q"))
		Expect(errOut).To(ContainSubstring("allocation_rule_override: never"))
		// Grid Engine's own reason survives, on its own line.
		Expect(errOut).To(ContainSubstring("no suitable queues"))
		Expect(errOut).NotTo(ContainSubstring("Exiting."), "the trailing line is noise mid-sentence")
	})

	It("names the single-node shape when $pe_slots was pinned", func() {
		errOut, _ := submitFailing(cfg, refusal, "-p", "gpu", "-N", "1", "-c", "8")
		Expect(errOut).To(ContainSubstring("1 node with all 8 slot(s)"))
	})

	// The regression this guards: exit code alone cannot tell a geometry refusal
	// from an unrelated failure, and blaming the node count for a dead qmaster
	// sends the user somewhere they cannot fix it.
	DescribeTable("passes every other failure through verbatim",
		func(geStderr string) {
			errOut, code := submitFailing(cfg, geStderr, "-p", "gpu", "-N", "3", "--ntasks-per-node=2")

			Expect(code).To(Equal(1))
			Expect(errOut).To(ContainSubstring(strings.TrimSpace(geStderr)))
			Expect(errOut).NotTo(ContainSubstring("Requested node configuration"))
			Expect(errOut).NotTo(ContainSubstring("allocation_rule_override"))
		},
		Entry("unreachable qmaster", "error: commlib error: got read error"),
		Entry("unknown queue", "Unable to run job: error: unknown queue \"nope.q\""),
		Entry("denied by an ACL", "Unable to run job: job rejected: user not allowed in queue"),
	)

	It("passes a refusal through verbatim when no rule was emitted", func() {
		// Same stderr, but the job carried no layout, so -w e was never sent and
		// the message cannot be about a geometry this shim pinned.
		errOut, code := submitFailing(cfg, refusal, "-p", "gpu", "-n", "16")

		Expect(code).To(Equal(1))
		Expect(errOut).NotTo(ContainSubstring("Requested node configuration"))
		Expect(errOut).To(ContainSubstring("no suitable queues"))
	})
})

var _ = Describe("wrapper-mode spool survives a submit and not a refusal [SI-57]", func() {
	wrapperCfg := func(spool string) *config.Config {
		cfg := testCfg()
		cfg.WrapperMode = true
		cfg.WrapperSpoolDir = spool
		return cfg
	}
	submitTo := func(cfg *config.Config, runner *fake.Runner) int {
		script := filepath.Join(GinkgoT().TempDir(), "job.sh")
		Expect(os.WriteFile(script, []byte("#!/bin/bash\n"), 0o700)).To(Succeed())
		var out, errOut bytes.Buffer
		return run(runner, cfg, "/usr/bin/slurm-shim",
			[]string{"-p", "gpu", "-N", "3", "--ntasks-per-node=2", script}, &out, &errOut)
	}

	// A -w e refusal is a routine outcome, not an exceptional one, so the spool it
	// leaves behind would accumulate: a submitit sweep that rejects 500 times would
	// leave 500 directories next to the user's script.
	It("removes the spool when qsub refuses the job", func() {
		spool := GinkgoT().TempDir()
		Expect(submitTo(wrapperCfg(spool),
			failingQsub(parUsage, "Unable to run job: error: no suitable queues.\n"))).To(Equal(1))

		entries, err := os.ReadDir(spool)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty(), "no job id exists, so nothing can reference the stored original")
	})

	It("keeps the spool when the job is accepted", func() {
		// The wrapper execs the stored original by path at run time, so a
		// successful submit must leave it in place.
		spool := GinkgoT().TempDir()
		var captured []string
		Expect(submitTo(wrapperCfg(spool), parRunner(parUsage, &captured))).To(Equal(0))

		entries, err := os.ReadDir(spool)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1), "the stored original must outlive the submit")
	})
})
