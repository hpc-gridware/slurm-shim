package sbatch

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
)

func TestSbatch(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Sbatch Suite")
}

func testCfg() *config.Config {
	c := config.Default()
	c.Partitions = map[string]config.Partition{
		"gpu":   {Queue: "gpu.q", PE: "gpu.pe", Slots: "per-task"},
		"batch": {Queue: "all.q", PE: "smp.pe", Slots: "16"},
	}
	return c
}

var _ = Describe("#SBATCH directive parsing [REQ-SBT-001]", func() {
	It("extracts directive tokens from the top of the script", func() {
		script := "#!/bin/bash\n#SBATCH --partition=gpu --nodes=2\n#SBATCH -n 8\n\nsrun hostname\n#SBATCH --late=ignored\n"
		Expect(ParseDirectives([]byte(script))).To(Equal([]string{"--partition=gpu", "--nodes=2", "-n", "8"}))
	})

	It("allows blank lines and ordinary comments between directives", func() {
		script := "#!/bin/bash\n# a comment\n#SBATCH -p batch\n\n#SBATCH -N 1\necho hi\n"
		Expect(ParseDirectives([]byte(script))).To(Equal([]string{"-p", "batch", "-N", "1"}))
	})
})

var _ = Describe("sbatch flag parsing [REQ-SBT-001]", func() {
	It("parses known long and short flags", func() {
		opt, warns, err := parseFlags([]string{"--partition=gpu", "-N", "2", "--ntasks-per-node=4", "-c", "2", "job.sh", "arg1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(warns).To(BeEmpty())
		Expect(opt.partition).To(Equal("gpu"))
		Expect(opt.nodes).To(Equal(2))
		Expect(opt.ntasksPerNode).To(Equal(4))
		Expect(opt.cpusPerTask).To(Equal(2))
		Expect(opt.script).To(Equal("job.sh"))
		Expect(opt.scriptArgs).To(Equal([]string{"arg1"}))
	})

	It("warns and ignores unknown directives including --container-* [REQ-SBT-001]", func() {
		opt, warns, err := parseFlags([]string{"--container-image=foo:latest", "--weird", "-p", "batch", "job.sh"})
		Expect(err).NotTo(HaveOccurred())
		Expect(warns).To(ContainElement(ContainSubstring("--container-image")))
		Expect(warns).To(ContainElement(ContainSubstring("--weird")))
		Expect(opt.partition).To(Equal("batch"))
		Expect(opt.script).To(Equal("job.sh"))
	})

	It("consumes a space-form value-taking --container-* flag so it is not taken as the script", func() {
		opt, _, err := parseFlags([]string{"--container-image", "foo:latest", "-p", "batch", "job.sh"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.script).To(Equal("job.sh"))
	})

	It("does not consume a token after a boolean --container-* flag", func() {
		opt, _, err := parseFlags([]string{"--container-readonly", "-p", "batch", "job.sh"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.partition).To(Equal("batch"))
		Expect(opt.script).To(Equal("job.sh"))
	})
})

var _ = Describe("quote-aware directive tokenizing [REQ-SBT-001]", func() {
	It("keeps a quoted value with spaces as one token", func() {
		script := "#!/bin/bash\n#SBATCH --job-name=\"my job\" -p gpu\nsrun x\n"
		Expect(ParseDirectives([]byte(script))).To(Equal([]string{"--job-name=my job", "-p", "gpu"}))
		opt, _, err := parseFlags(ParseDirectives([]byte(script)))
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.jobName).To(Equal("my job"))
	})

	It("handles single quotes", func() {
		Expect(tokenizeDirective("--comment 'hello world' -N 2")).To(Equal([]string{"--comment", "hello world", "-N", "2"}))
	})
})

var _ = Describe("wrapper shell escaping [REQ-SBT-004]", func() {
	It("escapes single quotes in embedded paths", func() {
		Expect(shellQuote("a'b")).To(Equal(`'a'\''b'`))
		w := buildWrapper("/opt/sh'im", "/a/b'c.sh")
		// The stored path is single-quote-escaped, so no bare quote breaks out.
		Expect(w).To(ContainSubstring(`exec '/a/b'\''c.sh'`))
		Expect(w).To(ContainSubstring(`'/opt/sh'\''im'`))
	})
})

var _ = Describe("slots rule + job id [REQ-SBT-002, REQ-SBT-003]", func() {
	It("computes per-task slots as ntasks x cpus_per_task", func() {
		opt, _, _ := parseFlags([]string{"-N", "2", "--ntasks-per-node=4", "-c", "2"})
		slots, err := computeSlots(opt, config.Partition{Slots: "per-task"})
		Expect(err).NotTo(HaveOccurred())
		Expect(slots).To(Equal(16)) // (2*4) tasks * 2 cpus
	})

	It("uses a literal integer slots rule verbatim", func() {
		opt, _, _ := parseFlags([]string{"-N", "2"})
		slots, err := computeSlots(opt, config.Partition{Slots: "16"})
		Expect(err).NotTo(HaveOccurred())
		Expect(slots).To(Equal(16))
	})

	It("defaults -N without -n to one task per node", func() {
		opt, _, _ := parseFlags([]string{"-N", "3"})
		slots, _ := computeSlots(opt, config.Partition{Slots: "per-task"})
		Expect(slots).To(Equal(3))
	})

	DescribeTable("parses the base id from qsub -terse",
		func(terse, want string) { Expect(parseJobID(terse)).To(Equal(want)) },
		Entry("plain", "4711\n", "4711"),
		Entry("array", "4711.1-4:1\n", "4711"),
		Entry("trailing space", "  4712  ", "4712"),
	)
})

var _ = Describe("sbatch SLURM defaults: submit-dir cwd + env forwarding", func() {
	submit := func(cliArgs ...string) []string {
		script := writeScriptTop2("#!/bin/bash\n#SBATCH -p batch\nsrun true\n")
		var captured []string
		rc := run(fakeQsub("80", &captured), testCfg(), "/shim", append(cliArgs, script), io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		return captured
	}

	It("defaults to the submit dir (-cwd) and full env forwarding (-V), like SLURM", func() {
		captured := submit()
		Expect(captured).To(ContainElement("-cwd"))
		Expect(captured).To(ContainElement("-V"))
	})

	It("defaults stdout to slurm-<jobid>.out and joins stderr into it, like SLURM", func() {
		captured := submit()
		Expect(captured).To(ContainElements("-o", "slurm-$JOB_ID.out"))
		Expect(captured).To(ContainElements("-j", "y"))
		Expect(captured).NotTo(ContainElement("-e"))
	})

	It("keeps stderr separate when --error is given (no -j y)", func() {
		captured := submit("--error=err.log")
		Expect(captured).To(ContainElements("-e", "err.log"))
		Expect(captured).NotTo(ContainElement("-j"))
	})

	It("rewrites a %a batch path into the user's directory for a 0-based array", func() {
		// GE's $TASK_ID is 1..N over the submitted range, so for a 0-based array
		// it is off by one: the path would hit another task's file, or a directory
		// that does not exist (Eqw). Keep the literal directory the user chose and
		// use a GE-expressible leaf -- never relocate output to the submit dir.
		captured := submit("--array=0-2", "--output=logs/%A_%a/%A_%a_0.out")
		Expect(captured).To(ContainElements("-o", "logs/slurm-$JOB_ID.$TASK_ID.out"))
	})

	It("rewrites --error and keeps the streams separate (no -j y)", func() {
		// Regression: an earlier form of this guard dropped -o, -e AND -j y, so GE
		// opened both cwd-relative defaults and Eqw-failed from an unwritable cwd.
		// The user asked for separate streams, so -j y must NOT appear.
		captured := submit("--array=0-2", "--output=logs/%a.out", "--error=logs/%a.err")
		Expect(captured).To(ContainElements("-o", "logs/slurm-$JOB_ID.$TASK_ID.out"))
		Expect(captured).To(ContainElements("-e", "logs/slurm-$JOB_ID.$TASK_ID.err"))
		Expect(captured).NotTo(ContainElement("-j"))
	})

	It("still merges into stdout when only --output was rewritten", func() {
		captured := submit("--array=0-2", "--output=logs/%a.out")
		Expect(captured).To(ContainElements("-o", "logs/slurm-$JOB_ID.$TASK_ID.out"))
		Expect(captured).To(ContainElements("-j", "y"))
		Expect(captured).NotTo(ContainElement("-e"))
	})

	It("warns once when a path is rewritten, and stays quiet when it is not", func() {
		// The substitution is user-visible: output does not land where asked, so
		// sbatch must say so -- exactly once, even when both -o and -e are rewritten.
		script := writeScriptTop2("#!/bin/bash\n#SBATCH -p batch\nsrun true\n")
		warnOn := func(args ...string) string {
			var errBuf bytes.Buffer
			var captured []string
			rc := run(fakeQsub("81", &captured), testCfg(), "/shim",
				append(args, script), io.Discard, &errBuf)
			Expect(rc).To(Equal(0))
			return errBuf.String()
		}

		out := warnOn("--array=0-2", "--output=logs/%a.out", "--error=logs/%a.err")
		Expect(out).To(ContainSubstring("Grid Engine cannot express %a"))
		Expect(strings.Count(out, "cannot express %a")).To(Equal(1), "warned more than once")

		// A 1-based unit-step array needs no substitution, so no warning.
		Expect(warnOn("--array=1-3", "--output=logs/%a.out")).NotTo(ContainSubstring("cannot express"))
	})

	It("keeps both streams coherent when only one path references %a", func() {
		captured := submit("--array=0-2", "--output=logs/%A.out", "--error=logs/%a.err")
		Expect(captured).To(ContainElements("-o", "logs/$JOB_ID.out"))
		Expect(captured).To(ContainElements("-e", "logs/slurm-$JOB_ID.$TASK_ID.err"))
	})

	It("never collapses -o and -e onto one file when rewriting", func() {
		// Same directory and extension must not merge two streams the user
		// deliberately separated (that would be an unrequested -j y).
		captured := submit("--array=0-2", "--output=logs/x_%a.log", "--error=logs/x_%a_err.log")
		Expect(captured).To(ContainElements("-o", "logs/slurm-$JOB_ID.$TASK_ID.out"))
		Expect(captured).To(ContainElements("-e", "logs/slurm-$JOB_ID.$TASK_ID.err"))
	})

	It("expands a zero-pad verb on an aligned array instead of leaving it literal", func() {
		// GE cannot pad, but %3a must still vary per task or every task shares
		// one literal filename.
		captured := submit("--array=1-9", "--output=logs/run_%3a.log")
		Expect(captured).To(ContainElements("-o", "logs/run_$TASK_ID.log"))
	})

	It("keeps a literal %% in the directory prefix when rewriting", func() {
		captured := submit("--array=0-2", "--output=100%%dir/%a.out")
		Expect(captured).To(ContainElements("-o", "100%dir/slurm-$JOB_ID.$TASK_ID.out"))
	})

	It("rewrites a stepped 1-based array too (GE tasks are dense, SLURM indices are not)", func() {
		// --array=1-9:2 is SLURM indices 1,3,5,7,9 but GE submits a dense -t 1-5.
		captured := submit("--array=1-9:2", "--output=logs/%a.out")
		Expect(captured).To(ContainElements("-o", "logs/slurm-$JOB_ID.$TASK_ID.out"))
	})

	It("treats the zero-pad form %3a as an array reference", func() {
		captured := submit("--array=0-99", "--output=logs/run_%3a.log")
		Expect(captured).To(ContainElements("-o", "logs/slurm-$JOB_ID.$TASK_ID.out"))
	})

	It("keeps a %a batch path for a 1-based array, where $TASK_ID lines up", func() {
		captured := submit("--array=1-3", "--output=logs/%A_%a.out")
		Expect(captured).To(ContainElements("-o", "logs/$JOB_ID_$TASK_ID.out"))
	})

	It("keeps a batch path that does not reference %a", func() {
		captured := submit("--array=0-2", "--output=logs/%A.out")
		Expect(captured).To(ContainElements("-o", "logs/$JOB_ID.out"))
	})

	It("does not force a default -o for arrays (GE per-task naming stands)", func() {
		captured := submit("--array=0-3")
		Expect(captured).NotTo(ContainElement("slurm-$JOB_ID.out"))
		Expect(captured).To(ContainElements("-j", "y"))
	})

	It("uses -wd (not -cwd) when --chdir is given", func() {
		captured := submit("--chdir=/scratch/run")
		Expect(captured).To(ContainElements("-wd", "/scratch/run"))
		Expect(captured).NotTo(ContainElement("-cwd"))
	})

	It("suppresses env forwarding with --export=NONE", func() {
		captured := submit("--export=NONE")
		Expect(captured).NotTo(ContainElement("-V"))
	})

	It("maps an explicit --export list to -v (no -V)", func() {
		captured := submit("--export=FOO=1,BAR=2")
		Expect(captured).NotTo(ContainElement("-V"))
		Expect(captured).To(ContainElements("-v", "FOO=1"))
		Expect(captured).To(ContainElements("-v", "BAR=2"))
	})

	It("composes ALL with extra assignments (--export=ALL,FOO=1)", func() {
		captured := submit("--export=ALL,FOO=1")
		Expect(captured).To(ContainElement("-V"))
		Expect(captured).To(ContainElements("-v", "FOO=1"))
	})
})

var _ = Describe("sbatch resource/signal/dependency mapping [submitit Phase 4]", func() {
	DescribeTable("parses --time to seconds",
		func(v string, want int) {
			s, err := parseSlurmTime(v)
			Expect(err).NotTo(HaveOccurred())
			Expect(s).To(Equal(want))
		},
		Entry("minutes", "60", 3600),
		Entry("MM:SS", "1:30", 90),
		Entry("HH:MM:SS", "2:00:00", 7200),
		Entry("D-HH", "1-00", 86400),
		Entry("D-HH:MM:SS", "1-01:00:00", 90000),
		Entry("explicit zero-day D-HH:MM", "0-12:30", 45000), // not MM:SS (would be 750)
	)

	DescribeTable("converts memory to GE suffixes",
		func(v, want string) { Expect(convertMem(v)).To(Equal(want)) },
		Entry("GB", "4GB", "4G"),
		Entry("MB", "512MB", "512M"),
		Entry("bare number is MB", "1024", "1024M"),
		Entry("single-letter kept", "8G", "8G"),
	)

	DescribeTable("extracts the gpu count from --gres",
		func(v string, wantN int, wantOK bool) {
			n, ok := gresGPUCount(v)
			Expect(ok).To(Equal(wantOK))
			if wantOK {
				Expect(n).To(Equal(wantN))
			}
		},
		Entry("gpu:2", "gpu:2", 2, true),
		Entry("typed gpu", "gpu:a100:4", 4, true),
		Entry("non-gpu gres", "mps:100", 0, false),
	)

	It("parses the --signal lead time", func() {
		Expect(parseSignalDelay("USR2@90")).To(Equal(90))
		Expect(parseSignalDelay("B:USR2@120")).To(Equal(120))
		Expect(parseSignalDelay("USR2")).To(Equal(0))
	})

	It("collects numeric dependency ids for -hold_jid", func() {
		Expect(dependencyIDs("afterok:12:13,afterany:14")).To(Equal([]string{"12", "13", "14"}))
	})

	It("emits -l h_rt/s_rt/mem/gpu and -hold_jid end to end", func() {
		body := "#!/bin/bash\n#SBATCH -p gpu\n#SBATCH --time=60 --signal=USR2@90\n" +
			"#SBATCH --mem=4GB --gpus-per-node=2 --dependency=afterok:99\nsrun true\n"
		script := writeScriptTop2(body)
		var captured []string
		var errBuf bytes.Buffer
		rc := run(fakeQsub("70", &captured), testCfg(), "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(0))
		// h_rt=3600, s_rt=3600-90=3510, mem via default h_vmem, gpu via default gpu complex.
		Expect(captured).To(ContainElements("-l", "h_rt=3600,s_rt=3510,h_vmem=4G,gpu=2"))
		// This spec is afterok, so the approximation must reach stderr here rather
		// than needing a second submission to assert it.
		Expect(errBuf.String()).To(ContainSubstring("afterok/aftercorr is approximated"))
		Expect(captured).To(ContainElements("-hold_jid", "99"))
		// --signal -> -notify -r y so GE sends SIGUSR2 before a kill/reschedule and
		// the job is rerunnable (submitit checkpoint-then-requeue).
		Expect(captured).To(ContainElements("-notify", "-r", "y"))
	})
})

// writeScriptTop2 writes a script body to a temp file and returns its path.
func writeScriptTop2(body string) string {
	path := filepath.Join(GinkgoT().TempDir(), "job.sh")
	Expect(os.WriteFile(path, []byte(body), 0o700)).To(Succeed())
	return path
}

var _ = Describe("sbatch GPU request translation [JAX]", func() {
	submitGPU := func(directives string) []string {
		script := writeScriptTop2("#!/bin/bash\n#SBATCH -p gpu\n" + directives + "\nsrun true\n")
		var captured []string
		rc := run(fakeQsub("90", &captured), testCfg(), "/shim", []string{script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		return captured
	}

	It("translates --gpus-per-node to the gres complex", func() {
		Expect(submitGPU("#SBATCH --gpus-per-node=4")).To(ContainElement(ContainSubstring("gpu=4")))
	})

	It("scales --gpus-per-task by tasks per node (was silently dropped -> zero GPUs)", func() {
		captured := submitGPU("#SBATCH --gpus-per-task=1 --ntasks-per-node=8")
		Expect(captured).To(ContainElement(ContainSubstring("gpu=8")))
	})

	It("defaults --gpus-per-task to one task per node when no geometry is given", func() {
		Expect(submitGPU("#SBATCH --gpus-per-task=2")).To(ContainElement(ContainSubstring("gpu=2")))
	})

	It("derives tasks-per-node from --ntasks/--nodes when --ntasks-per-node is absent", func() {
		// 8 tasks over 2 nodes = 4 per node, so 4 GPUs per node -- not 1.
		captured := submitGPU("#SBATCH --nodes=2 --ntasks=8 --gpus-per-task=1")
		Expect(captured).To(ContainElement(ContainSubstring("gpu=4")))
	})

	It("publishes SLURM_GPU_BIND=per_task for --gpus-per-task so the step binds", func() {
		captured := submitGPU("#SBATCH --gpus-per-task=1 --ntasks-per-node=8")
		Expect(captured).To(ContainElements("-v", "SLURM_GPU_BIND=per_task:1"))
		Expect(captured).To(ContainElements("-v", "SLURM_GPUS_PER_TASK=1"))
	})

	It("passes an explicit #SBATCH --gpu-bind through to the step", func() {
		captured := submitGPU("#SBATCH --gpus-per-node=8 --gpu-bind=none")
		Expect(captured).To(ContainElements("-v", "SLURM_GPU_BIND=none"))
	})

	It("requests no GPUs when nothing asked for them", func() {
		captured := submitGPU("#SBATCH -N 1")
		for _, a := range captured {
			Expect(a).NotTo(ContainSubstring("gpu="))
		}
	})
})

var _ = Describe("sbatch output-path translation [submitit Phase 2]", func() {
	DescribeTable("rewrites SLURM %-verbs to GE pseudo-variables",
		func(in, want string) { Expect(translateOutputPath(in)).To(Equal(want)) },
		Entry("single job id + task", "logs/%j_%t_log.out", "logs/$JOB_ID_0_log.out"),
		Entry("array job/task", "logs/%A_%a_%t_log.out", "logs/$JOB_ID_$TASK_ID_0_log.out"),
		Entry("job name and user", "%x-%u.out", "$JOB_NAME-$USER.out"),
		Entry("node name", "%N.err", "$HOSTNAME.err"),
		Entry("literal percent", "100%%.out", "100%.out"),
		Entry("unknown verb kept", "%z.out", "%z.out"),
	)

	It("passes translated -o/-e through to qsub", func() {
		script := writeScriptTop("#!/bin/bash\n#SBATCH -p batch\n#SBATCH -o %j_%t_log.out\n#SBATCH -e %j_%t_log.err\nsrun true\n")
		var captured []string
		rc := run(fakeQsub("55", &captured), testCfg(), "/shim", []string{script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(captured).To(ContainElements("-o", "$JOB_ID_0_log.out"))
		Expect(captured).To(ContainElements("-e", "$JOB_ID_0_log.err"))
	})
})

// writeScriptTop writes a script body to a temp file and returns its path.
func writeScriptTop(body string) string {
	path := filepath.Join(GinkgoT().TempDir(), "job.sh")
	Expect(os.WriteFile(path, []byte(body), 0o700)).To(Succeed())
	return path
}

var _ = Describe("sbatch --array translation [submitit Phase 3]", func() {
	DescribeTable("parses --array specs",
		func(spec string, wantMin, wantMax, wantStep, wantThrottle int) {
			opt, _, err := parseFlags([]string{"--array=" + spec, "job.sh"})
			Expect(err).NotTo(HaveOccurred())
			Expect(opt.haveArray).To(BeTrue())
			Expect([]int{opt.arrayMin, opt.arrayMax, opt.arrayStep, opt.arrayThrottle}).
				To(Equal([]int{wantMin, wantMax, wantStep, wantThrottle}))
		},
		Entry("submitit 0-based with throttle", "0-9%4", 0, 9, 1, 4),
		Entry("no throttle", "0-3", 0, 3, 1, 0),
		Entry("stepped", "0-10:2", 0, 10, 2, 0),
		Entry("single element", "5", 5, 5, 1, 0),
		Entry("non-zero origin", "5-10", 5, 10, 1, 0),
	)

	DescribeTable("rejects unsupported specs",
		func(spec string) {
			_, _, err := parseFlags([]string{"--array=" + spec, "job.sh"})
			Expect(err).To(HaveOccurred())
		},
		Entry("comma list", "0,2,4"),
		Entry("bad range", "5-1"),
		Entry("bad throttle", "0-9%x"),
		Entry("bad step", "0-9:0"),
	)

	writeScript := func(body string) string {
		path := filepath.Join(GinkgoT().TempDir(), "job.sh")
		Expect(os.WriteFile(path, []byte(body), 0o700)).To(Succeed())
		return path
	}

	It("submits a dense GE -t range plus -tc and the SLURM base/step hints", func() {
		script := writeScript("#!/bin/bash\n#SBATCH -p batch\n#SBATCH --array=0-9%4\nsrun true\n")
		var captured []string
		rc := run(fakeQsub("4712", &captured), testCfg(), "/shim", []string{script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(captured).To(ContainElements("-t", "1-10", "-tc", "4"))
		Expect(captured).To(ContainElements("-v", "SLURM_ARRAY_BASE=0"))
		Expect(captured).To(ContainElements("-v", "SLURM_ARRAY_STEP=1"))
	})

	It("maps a stepped SLURM array to a dense GE range of the right size", func() {
		script := writeScript("#!/bin/bash\n#SBATCH -p batch\n#SBATCH --array=0-10:2\nsrun true\n")
		var captured []string
		rc := run(fakeQsub("4713", &captured), testCfg(), "/shim", []string{script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(captured).To(ContainElements("-t", "1-6")) // 0,2,4,6,8,10 -> 6 tasks
		Expect(captured).To(ContainElements("-v", "SLURM_ARRAY_STEP=2"))
		Expect(captured).NotTo(ContainElement("-tc")) // no throttle given
	})
})

// fakeQsub captures the qsub argv and returns a terse id.
func fakeQsub(id string, capture *[]string) *fake.Runner {
	return &fake.Runner{Responder: func(name string, args []string) fake.Response {
		Expect(name).To(Equal("qsub"))
		*capture = append([]string{}, args...)
		return fake.Response{Stdout: []byte(id + "\n")}
	}}
}

var _ = Describe("sbatch end-to-end [REQ-SBT-002/003/005]", func() {
	writeScript := func(body string) string {
		path := filepath.Join(GinkgoT().TempDir(), "job.sh")
		Expect(os.WriteFile(path, []byte(body), 0o700)).To(Succeed())
		return path
	}

	It("translates a partition to -q/-pe/slots and prints the SLURM line", func() {
		script := writeScript("#!/bin/bash\n#SBATCH --partition=gpu\n#SBATCH -N 2 --ntasks-per-node=4\nsrun hostname\n")
		var captured []string
		var out bytes.Buffer
		rc := run(fakeQsub("4711", &captured), testCfg(), "/shim", []string{script}, &out, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(out.String()).To(Equal("Submitted batch job 4711\n"))
		Expect(captured).To(ContainElements("-terse", "-q", "gpu.q", "-pe", "gpu.pe", "8"))
		Expect(captured[len(captured)-1]).To(Equal(script)) // PE mode submits script as-is
	})

	It("lets a command-line partition override the directive", func() {
		script := writeScript("#!/bin/bash\n#SBATCH --partition=gpu\nsrun hostname\n")
		var captured []string
		rc := run(fakeQsub("5", &captured), testCfg(), "/shim", []string{"-p", "batch", script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(captured).To(ContainElements("-q", "all.q", "-pe", "smp.pe", "16"))
	})

	It("falls back to default_partition when none is given", func() {
		script := writeScript("#!/bin/bash\nsrun hostname\n")
		cfg := testCfg()
		cfg.DefaultPartition = "batch"
		var captured []string
		rc := run(fakeQsub("7", &captured), cfg, "/shim", []string{script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(captured).To(ContainElements("-q", "all.q", "-pe", "smp.pe", "16"))
	})

	It("errors when no partition is given and no default is configured", func() {
		script := writeScript("#!/bin/bash\nsrun hostname\n")
		var captured []string
		var errBuf bytes.Buffer
		rc := run(fakeQsub("8", &captured), testCfg(), "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("no partition specified"))
	})

	It("lets an explicit -p win over default_partition", func() {
		script := writeScript("#!/bin/bash\nsrun hostname\n")
		cfg := testCfg()
		cfg.DefaultPartition = "batch"
		var captured []string
		rc := run(fakeQsub("9", &captured), cfg, "/shim", []string{"-p", "gpu", script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		Expect(captured).To(ContainElements("-q", "gpu.q", "-pe", "gpu.pe")) // default ignored
	})

	It("errors with 'unknown partition' when default_partition is invalid", func() {
		script := writeScript("#!/bin/bash\nsrun hostname\n")
		cfg := testCfg()
		cfg.DefaultPartition = "ghost"
		var captured []string
		var errBuf bytes.Buffer
		rc := run(fakeQsub("10", &captured), cfg, "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("unknown partition"))
	})

	It("surfaces a qsub failure and exits non-zero [REQ-SBT-005]", func() {
		script := writeScript("#!/bin/bash\n#SBATCH -p gpu\n")
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("qsub: no such PE")}
		}}
		var errBuf bytes.Buffer
		rc := run(r, testCfg(), "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("no such PE"))
	})

	It("errors on an unknown partition", func() {
		script := writeScript("#!/bin/bash\n#SBATCH -p nope\n")
		var errBuf bytes.Buffer
		rc := run(&fake.Runner{}, testCfg(), "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(1))
		Expect(errBuf.String()).To(ContainSubstring("unknown partition"))
	})

	It("submits a wrapper that execs the stored script verbatim in wrapper mode [REQ-SBT-004]", func() {
		body := "#!/usr/bin/env python\nprint('hi')\n"
		script := writeScript(body)
		cfg := testCfg()
		cfg.WrapperMode = true
		var captured []string
		rc := run(fakeQsub("9", &captured), cfg, "/opt/shim", []string{"-p", "batch", script}, io.Discard, io.Discard)
		Expect(rc).To(Equal(0))
		submitted := captured[len(captured)-1]
		Expect(filepath.Base(submitted)).To(Equal("wrapper.sh"))
		w, err := os.ReadFile(submitted)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(w)).To(ContainSubstring("slurm-shim-env --export"))
		Expect(string(w)).To(ContainSubstring("exec '"))
		// The stored original is byte-identical to the user script.
		stored := filepath.Join(filepath.Dir(submitted), "orig.sh")
		got, err := os.ReadFile(stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(body))
	})
})

var _ = Describe("unmappable and approximated flags warn instead of going quiet [REQ-SBT-001]", func() {
	It("names the Grid Engine equivalent for --exclusive rather than 'unknown directive'", func() {
		_, warns, err := parseFlags([]string{"--exclusive", "job.sh"})
		Expect(err).NotTo(HaveOccurred())
		Expect(warns).To(HaveLen(1))
		Expect(warns[0]).To(ContainSubstring("allocation_rule $pe_slots"))
		Expect(warns[0]).NotTo(ContainSubstring("unknown directive"))
		// Since OCS 9.1.5 the shim pins slots-per-node per job, so the old claim
		// that whole-node allocation can only be site configuration is false.
		Expect(warns[0]).NotTo(ContainSubstring("not a submission flag"))
		Expect(warns[0]).To(ContainSubstring("--ntasks-per-node"))
	})

	DescribeTable("--exclusive never consumes a token, in any spelling",
		// The arity trap: listing it in knownLong would mean "takes a value" and
		// swallow whatever follows.
		func(tokens []string, wantScript, wantWrap string) {
			opt, warns, err := parseFlags(tokens)
			Expect(err).NotTo(HaveOccurred())
			Expect(warns).To(HaveLen(1))
			Expect(opt.script).To(Equal(wantScript))
			Expect(opt.wrap).To(Equal(wantWrap))
		},
		Entry("before the script", []string{"--exclusive", "job.sh"}, "job.sh", ""),
		Entry("with a value", []string{"--exclusive=user", "job.sh"}, "job.sh", ""),
		Entry("before --wrap", []string{"--exclusive", "--wrap=echo hi"}, "", "echo hi"),
		Entry("among other flags", []string{"--exclusive", "-N", "2", "job.sh"}, "job.sh", ""),
	)

	DescribeTable("reports what a --dependency loses in translation",
		func(spec string, want string) {
			got := dependencyWarning(spec, dependencyIDs(spec))
			if want == "" {
				Expect(got).To(BeEmpty())
				return
			}
			Expect(got).To(ContainSubstring(want))
		},
		Entry("afterany is the one exact form", "afterany:99", ""),
		Entry("after is start-gated in SLURM, completion-gated here", "after:99", "start-gated"),
		Entry("afterok cannot be success-gated", "afterok:99", "approximated"),
		Entry("aftercorr loses per-element pairing too", "aftercorr:99", "approximated"),
		Entry("afternotok is inverted outright", "afternotok:99", "cannot be expressed"),
		Entry("case is ignored", "AfterOK:99", "approximated"),
		Entry("the worst clause wins", "afterany:1,afternotok:2", "cannot be expressed"),
		// Nothing to hold on: the job is not gated at all, which is worse than an
		// approximated gate and must not be the quiet case.
		Entry("singleton has no id", "singleton", "NOT held"),
		Entry("time-offset form", "after:99+10", "NOT held"),
		Entry("array-element id", "afterok:12345_3", "NOT held"),
		Entry("empty expansion", "afterok:", "NOT held"),
	)

	It("keeps both ids across SLURM's OR separator", func() {
		// '?' was missing from the split set, so the id fused to it was dropped
		// and the job silently held on the wrong predecessor set.
		Expect(dependencyIDs("afterok:11?afterany:12")).To(Equal([]string{"11", "12"}))
	})

	It("lets a command-line --dependency replace the directive, not add to it", func() {
		opt, _, err := parseFlags([]string{"--dependency=afterok:100", "--dependency=afterany:200", "job.sh"})
		Expect(err).NotTo(HaveOccurred())
		Expect(opt.holdJIDs).To(Equal([]string{"200"}), "last wins, as everywhere else")
		Expect(dependencyWarning(opt.depSpec, opt.holdJIDs)).To(BeEmpty(),
			"the superseded afterok must not still be warned about")
	})

	It("emits no -hold_jid and says so when nothing can be held", func() {
		body := "#!/bin/bash\n#SBATCH -p batch\n#SBATCH --dependency=singleton\nsrun true\n"
		script := writeScriptTop2(body)
		var captured []string
		var errBuf bytes.Buffer
		rc := run(fakeQsub("73", &captured), testCfg(), "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(0))
		Expect(captured).NotTo(ContainElement("-hold_jid"))
		Expect(errBuf.String()).To(ContainSubstring("NOT held"))
	})

	It("stays quiet for a dependency form GE expresses exactly", func() {
		body := "#!/bin/bash\n#SBATCH -p batch\n#SBATCH --dependency=afterany:99\nsrun true\n"
		script := writeScriptTop2(body)
		var captured []string
		var errBuf bytes.Buffer
		rc := run(fakeQsub("72", &captured), testCfg(), "/shim", []string{script}, io.Discard, &errBuf)
		Expect(rc).To(Equal(0))
		Expect(captured).To(ContainElements("-hold_jid", "99"))
		Expect(errBuf.String()).NotTo(ContainSubstring("dependency"))
	})
})

// config.Parse returns a nil *Config on a hard error, so discarding that error --
// which Run did until the allocation-rule work needed load-time validation --
// turned a config typo into a nil dereference at the first cfg field access.
var _ = Describe("Run surfaces config failures instead of dereferencing nil", func() {
	writeConfig := func(body string) string {
		p := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		Expect(os.WriteFile(p, []byte(body), 0o600)).To(Succeed())
		return p
	}

	// An unusable slot rule warns at load and fails only the submission that names
	// that partition -- config.Load is shared with squeue, sinfo, srun and the PE
	// hook, so it must not be fatal there.
	It("warns about a bad slot rule and fails only the submission that uses it", func() {
		GinkgoT().Setenv("SLURM_SHIM_CONFIG",
			writeConfig("partitions:\n  batch: {queue: all.q, pe: make, slots: \"0\"}\n"+
				"  good: {queue: all.q, pe: make, slots: \"per-task\"}\n"))
		var out, errOut bytes.Buffer

		var code int
		Expect(func() { code = Run([]string{"-p", "batch", "job.sh"}, &out, &errOut) }).NotTo(Panic())

		Expect(code).To(Equal(1))
		Expect(errOut.String()).To(ContainSubstring("sbatch: warning:"))
		Expect(errOut.String()).To(ContainSubstring("slots rule"))
		Expect(errOut.String()).To(ContainSubstring("partition \"batch\""))
		Expect(errOut.String()).NotTo(ContainSubstring("loading config"),
			"one partition's typo is not a config-load failure")
	})

	It("reports malformed YAML the same way", func() {
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", writeConfig("partitions: [oops\n"))
		var out, errOut bytes.Buffer

		var code int
		Expect(func() { code = Run(nil, &out, &errOut) }).NotTo(Panic())

		Expect(code).To(Equal(1))
		Expect(errOut.String()).To(ContainSubstring("sbatch: error: loading config"))
	})

	// The warnings were discarded by the same statement, so an unknown key was
	// computed and then thrown away -- the one signal a typo'd key ever produces.
	It("surfaces a config warning that used to be swallowed", func() {
		GinkgoT().Setenv("SLURM_SHIM_CONFIG", writeConfig("allocation_rule_overide: never\n"))
		var out, errOut bytes.Buffer

		// No script: the run stops right after the warning, so nothing is submitted.
		Expect(Run(nil, &out, &errOut)).To(Equal(1))
		Expect(errOut.String()).To(ContainSubstring("sbatch: warning: unknown config key"))
		Expect(errOut.String()).To(ContainSubstring("allocation_rule_overide"))
	})
})
