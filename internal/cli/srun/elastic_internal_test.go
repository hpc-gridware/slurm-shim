package srun

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata/fake"
	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

var _ = Describe("hot spares: which steps are elastic", func() {
	DescribeTable("recognizes torchrun",
		func(cmd []string, want bool) { Expect(isTorchrun(cmd)).To(Equal(want)) },
		Entry("torchrun", []string{"torchrun", "--nnodes=4", "train.py"}, true),
		Entry("torchrun by path", []string{"/opt/venv/bin/torchrun", "train.py"}, true),
		Entry("python -m torch.distributed.run", []string{"python3", "-m", "torch.distributed.run", "train.py"}, true),
		Entry("plain python", []string{"python3", "train.py"}, false),
		Entry("hostname", []string{"hostname"}, false),
		Entry("empty", []string{}, false),
	)

	withSpares := func(opt *options) *supervisor {
		return &supervisor{
			cfg: config.Default(),
			opt: opt,
			lay: &layout.Layout{Spares: []layout.Node{{Host: "node003"}}},
		}
	}

	It("is elastic by default only for torchrun steps in a job with spares", func() {
		Expect(withSpares(&options{command: []string{"torchrun", "x.py"}}).elasticStep()).To(BeTrue())
		Expect(withSpares(&options{command: []string{"hostname"}}).elasticStep()).To(BeFalse())
		s := withSpares(&options{command: []string{"torchrun", "x.py"}})
		s.lay.Spares = nil
		Expect(s.elasticStep()).To(BeFalse(), "no spares, nothing to swap in")
	})

	It("lets --x-elastic win over the job's SLURM_X_ELASTIC, and that over the site config", func() {
		GinkgoT().Setenv("SLURM_X_ELASTIC", "on")
		Expect(withSpares(&options{command: []string{"hostname"}}).elasticStep()).To(BeTrue())
		Expect(withSpares(&options{command: []string{"torchrun"}, elastic: "off"}).elasticStep()).To(BeFalse())

		GinkgoT().Setenv("SLURM_X_ELASTIC", "")
		s := withSpares(&options{command: []string{"hostname"}})
		s.cfg.Elastic = config.ElasticOn
		Expect(s.elasticStep()).To(BeTrue())
	})

	It("never replaces the master, a node that already reported, or during termination", func() {
		s := withSpares(&options{})
		s.elastic = true
		s.plan = &plan.StepPlan{Nodes: []plan.StepNode{{Host: "node001", LayoutIndex: 0}, {Host: "node002", LayoutIndex: 1}}}
		Expect(s.canReplace("node002", 0)).To(BeTrue())
		Expect(s.canReplace("node001", 0)).To(BeFalse(), "the master runs srun itself")
		Expect(s.canReplace("node002", 1)).To(BeFalse(), "a task already reported")
		s.terminating.Store(true)
		Expect(s.canReplace("node002", 0)).To(BeFalse(), "srun is stopping the step")
	})

	It("renders rank ranges", func() {
		Expect(rankRange([]int{4, 5, 6, 7})).To(Equal("4-7"))
		Expect(rankRange([]int{3})).To(Equal("3"))
		Expect(rankRange([]int{0, 2, 5})).To(Equal("0,2,5"))
	})
})

var _ = Describe("hot spares: taking a spare", func() {
	var tmp string
	BeforeEach(func() {
		tmp = GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		Expect(layout.Write(filepath.Join(tmp, layout.StateDir), &layout.Layout{
			SchemaVersion: layout.SchemaVersion,
			Nodes:         []layout.Node{{Index: 0, Host: "node001", IsMaster: true}, {Index: 1, Host: "node002"}},
			Spares:        []layout.Node{{Index: 0, Host: "node003", GPUs: []string{"2"}}},
		})).To(Succeed())
	})
	sup := func(step int) *supervisor { return &supervisor{lay: &layout.Layout{}, stepID: step} }

	It("moves the spare into the lost node's index and records the swap", func() {
		spare, used, total, err := sup(3).takeSpare("node002")
		Expect(err).NotTo(HaveOccurred())
		Expect(spare.Host).To(Equal("node003"))
		Expect(spare.Index).To(Equal(1))
		Expect(spare.GPUs).To(Equal([]string{"2"}), "the spare keeps its own grant")
		Expect([]int{used, total}).To(Equal([]int{1, 1}))

		l, err := layout.Read(filepath.Join(tmp, layout.StateDir, layout.LayoutFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Nodes[1].Host).To(Equal("node003"))
		Expect(l.Spares).To(BeEmpty())
		Expect(l.Lost).To(Equal([]string{"node002"}))
		Expect(l.Swaps).To(ConsistOf(HaveField("Step", 3)))
	})

	It("reuses another step's replacement instead of consuming a second spare", func() {
		// Two concurrent steps lose the same node: both must land on one spare.
		first, _, _, err := sup(1).takeSpare("node002")
		Expect(err).NotTo(HaveOccurred())
		second, _, _, err := sup(2).takeSpare("node002")
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Host).To(Equal(first.Host))
	})

	It("reports exhaustion", func() {
		_, _, _, err := sup(1).takeSpare("node002")
		Expect(err).NotTo(HaveOccurred())
		_, used, total, err := sup(1).takeSpare("node003")
		Expect(err).To(MatchError(errNoSpare))
		Expect([]int{used, total}).To(Equal([]int{1, 1}))
	})
})

var _ = Describe("hot spares: a node lost before the step starts [AC6]", func() {
	It("moves the node to a spare at launch instead of failing the step", func() {
		tmp := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		Expect(layout.Write(filepath.Join(tmp, layout.StateDir), &layout.Layout{
			SchemaVersion: layout.SchemaVersion,
			Nodes:         []layout.Node{{Index: 0, Host: "node001", IsMaster: true}, {Index: 1, Host: "node002"}},
			Spares:        []layout.Node{{Index: 0, Host: "node003"}},
		})).To(Succeed())
		stderr := gbytes.NewBuffer()
		s := &supervisor{
			stderr: stderr, elastic: true, lay: &layout.Layout{}, opt: &options{}, cfg: config.Default(),
			plan: &plan.StepPlan{
				Nodes: []plan.StepNode{{Host: "node001", LayoutIndex: 0}, {Host: "node002", LayoutIndex: 1}},
				Ranks: []plan.PlacedRank{{Rank: 0, StepNodeIndex: 0}, {Rank: 1, StepNodeIndex: 1}},
			},
		}
		var tried []string
		_, err := startAt(s, 1, func(ni int) (launch.Handle, error) {
			host := s.plan.Nodes[ni].Host
			tried = append(tried, host)
			if host == "node002" {
				return nil, launch.HostError(errors.New("qrsh: node002 never accepted the task"))
			}
			return exitedHandle(host), nil
		})

		Expect(err).NotTo(HaveOccurred())
		Expect(tried).To(Equal([]string{"node002", "node003"}))
		Expect(s.plan.Nodes[1].Host).To(Equal("node003"))
		Expect(string(stderr.Contents())).To(ContainSubstring("tasks 1 placed on node003 (1/1 spares used)"))
	})

	It("gives up when no spare is left, as without spares", func() {
		tmp := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		Expect(layout.Write(filepath.Join(tmp, layout.StateDir), &layout.Layout{
			SchemaVersion: layout.SchemaVersion,
			Nodes:         []layout.Node{{Index: 0, Host: "node001", IsMaster: true}, {Index: 1, Host: "node002"}},
		})).To(Succeed())
		s := &supervisor{
			stderr: gbytes.NewBuffer(), elastic: true, lay: &layout.Layout{}, opt: &options{}, cfg: config.Default(),
			plan: &plan.StepPlan{Nodes: []plan.StepNode{{Host: "node001"}, {Host: "node002", LayoutIndex: 1}}},
		}
		_, err := startAt(s, 1, func(int) (launch.Handle, error) {
			return nil, launch.HostError(errors.New("down"))
		})
		Expect(err).To(MatchError("down"))
	})
})

var _ = Describe("hot spares: torchrun advice for an elastic step", func() {
	sup := func(cmd ...string) *supervisor {
		return &supervisor{
			elastic: true,
			opt:     &options{command: cmd},
			lay: &layout.Layout{
				Nodes:      []layout.Node{{Host: "node001"}, {Host: "node002"}},
				Spares:     []layout.Node{{Host: "node003"}},
				Rendezvous: layout.Rendezvous{MasterAddr: "node001"},
			},
			plan: &plan.StepPlan{Nodes: []plan.StepNode{{Host: "node001"}, {Host: "node002"}}},
		}
	}

	It("is silent for a well-configured step", func() {
		Expect(sup("torchrun", "--nnodes=2", "--max-restarts=3", "--rdzv-backend=c10d",
			"--rdzv-endpoint=node001:29500", "train.py", "--lr", "0.1").torchrunWarnings()).To(BeEmpty())
	})

	It("accepts underscores, space-separated values and $MASTER_ADDR", func() {
		Expect(sup("python3", "-m", "torch.distributed.run", "--nnodes", "2", "--max_restarts", "1",
			"--rdzv_backend", "c10d", "--rdzv_endpoint", "$MASTER_ADDR:29500", "train.py").torchrunWarnings()).To(BeEmpty())
	})

	It("does not read a boolean flag's next word as its value", func() {
		w := sup("torchrun", "--standalone", "train.py", "--max-restarts=9").torchrunWarnings()
		Expect(w).To(ContainElement(ContainSubstring("--max-restarts=(unset)")), "the script's own args are not torchrun's")
	})

	It("warns about each setting that blocks a replacement", func() {
		w := sup("torchrun", "--nnodes=4", "--rdzv-backend=static", "--rdzv-endpoint=node002:29500", "train.py").torchrunWarnings()
		Expect(w).To(ContainElement(ContainSubstring("--max-restarts=(unset) is below the job's 1 spare(s)")))
		Expect(w).To(ContainElement(ContainSubstring("--rdzv-backend=static cannot take a replacement node")))
		Expect(w).To(ContainElement(ContainSubstring("is not on the master host node001")))
		Expect(w).To(ContainElement(ContainSubstring("--nnodes=4 does not match the step's 2 node(s)")))
	})

	It("says nothing for a step that is not elastic", func() {
		s := sup("torchrun", "train.py")
		s.elastic = false
		Expect(s.torchrunWarnings()).To(BeEmpty())
	})

	It("warns when the user turns torchrun's shared rendezvous store back on", func() {
		GinkgoT().Setenv(shareStoreVar, "0")
		w := sup("torchrun", "--nnodes=2", "--max-restarts=3", "--rdzv-backend=c10d", "train.py").torchrunWarnings()
		Expect(w).To(ConsistOf(ContainSubstring("TORCH_DISABLE_SHARE_RDZV_TCP_STORE=0")))
	})
})

var _ = Describe("hot spares: the elastic torchrun environment", func() {
	sup := func(elastic bool, cmd ...string) *supervisor {
		return &supervisor{elastic: elastic, opt: &options{command: cmd}}
	}

	It("opts an elastic torchrun step out of the shared rendezvous store", func() {
		Expect(sup(true, "torchrun", "x.py").withElasticEnv([]string{"A=1"})).
			To(Equal([]string{"A=1", "TORCH_DISABLE_SHARE_RDZV_TCP_STORE=1"}))
	})

	It("keeps the user's own setting", func() {
		Expect(sup(true, "torchrun", "x.py").withElasticEnv([]string{"TORCH_DISABLE_SHARE_RDZV_TCP_STORE=0"})).
			To(Equal([]string{"TORCH_DISABLE_SHARE_RDZV_TCP_STORE=0"}))
	})

	It("leaves other steps alone", func() {
		Expect(sup(false, "torchrun", "x.py").withElasticEnv([]string{"A=1"})).To(Equal([]string{"A=1"}))
		Expect(sup(true, "python3", "x.py").withElasticEnv([]string{"A=1"})).To(Equal([]string{"A=1"}))
	})
})

var _ = Describe("hot spares: the relaunched node's environment [AC8]", func() {
	It("names the spare in the step node list and gives its ranks only the spare's GPUs", func() {
		s := &supervisor{
			cfg: config.Default(),
			opt: &options{req: plan.StepRequest{GPUsPerTask: 1}},
			lay: &layout.Layout{},
			plan: &plan.StepPlan{
				NTasks: 2,
				Nodes:  []plan.StepNode{{Host: "node001", GPUs: []string{"0"}}, {Host: "node002", LayoutIndex: 1, GPUs: []string{"1"}}},
				Ranks:  []plan.PlacedRank{{Rank: 0, StepNodeIndex: 0, GPUs: []string{"0"}}, {Rank: 1, StepNodeIndex: 1, GPUs: []string{"1"}}},
			},
		}
		Expect(s.baseEnv()).To(ContainElement("SLURM_STEP_NODELIST=node[001-002]"))

		s.placeOnSpare(1, layout.Node{Index: 1, Host: "node003", GPUs: []string{"2"}})
		spec := s.relaunchSpec(1)

		Expect(spec.Env).To(ContainElement("SLURM_STEP_NODELIST=node[001,003]"))
		Expect(spec.Env).NotTo(ContainElement("SLURM_STEP_NODELIST=node[001-002]"))
		Expect(spec.AppendOutput).To(BeTrue())
		Expect(spec.Ranks).To(HaveLen(1))
		Expect(spec.Ranks[0].Rank).To(Equal(1))
		Expect([]string(spec.Ranks[0].GPUs)).To(Equal([]string{"2"}), "the spare's own grant, not the lost node's")
	})
})

var _ = Describe("hot spares: a dead spare at launch [AC5]", func() {
	It("moves on to the next spare", func() {
		tmp := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		Expect(layout.Write(filepath.Join(tmp, layout.StateDir), &layout.Layout{
			SchemaVersion: layout.SchemaVersion,
			Nodes:         []layout.Node{{Index: 0, Host: "node001", IsMaster: true}, {Index: 1, Host: "node002"}},
			Spares:        []layout.Node{{Index: 0, Host: "node003"}, {Index: 1, Host: "node004"}},
		})).To(Succeed())
		stderr := gbytes.NewBuffer()
		s := &supervisor{
			stderr: stderr, elastic: true, lay: &layout.Layout{}, opt: &options{}, cfg: config.Default(),
			plan: &plan.StepPlan{
				Nodes: []plan.StepNode{{Host: "node001", LayoutIndex: 0}, {Host: "node002", LayoutIndex: 1}},
				Ranks: []plan.PlacedRank{{Rank: 0, StepNodeIndex: 0}, {Rank: 1, StepNodeIndex: 1}},
			},
		}
		var tried []string
		_, err := startAt(s, 1, func(ni int) (launch.Handle, error) {
			host := s.plan.Nodes[ni].Host
			tried = append(tried, host)
			if host != "node004" {
				return nil, launch.HostError(errors.New("qrsh: " + host + " never accepted the task"))
			}
			return exitedHandle(host), nil
		})

		Expect(err).NotTo(HaveOccurred())
		Expect(tried).To(Equal([]string{"node002", "node003", "node004"}))
		Expect(string(stderr.Contents())).To(ContainSubstring("node node003 did not start"))
		Expect(string(stderr.Contents())).To(ContainSubstring("tasks 1 placed on node004 (2/2 spares used)"))
	})
})

// writeSparesLayout writes a 2-node layout with the given spares and swaps into
// a fresh TMPDIR and returns the state directory.
func writeSparesLayout(nodes []layout.Node, spares []layout.Node, swaps []layout.Swap) string {
	tmp := GinkgoT().TempDir()
	GinkgoT().Setenv("TMPDIR", tmp)
	dir := filepath.Join(tmp, layout.StateDir)
	Expect(layout.Write(dir, &layout.Layout{
		SchemaVersion: layout.SchemaVersion,
		Job:           layout.Job{JobID: 42},
		Nodes:         nodes,
		Spares:        spares,
		Swaps:         swaps,
	})).To(Succeed())
	return dir
}

var _ = Describe("hot spares: only a real node loss costs spares", func() {
	nodes := []layout.Node{{Index: 0, Host: "node001", IsMaster: true}, {Index: 1, Host: "node002"}}
	elastic := func(stderr *gbytes.Buffer) *supervisor {
		return &supervisor{
			stderr: stderr, elastic: true, lay: &layout.Layout{}, opt: &options{}, cfg: config.Default(),
			plan: &plan.StepPlan{
				Nodes: []plan.StepNode{{Host: "node001"}, {Host: "node002", LayoutIndex: 1}},
				Ranks: []plan.PlacedRank{{Rank: 0, StepNodeIndex: 0}, {Rank: 1, StepNodeIndex: 1}},
			},
		}
	}

	It("does not swap at launch on an error that would repeat on every host", func() {
		dir := writeSparesLayout(nodes, []layout.Node{{Host: "node003"}}, nil)
		var tried []string
		s := elastic(gbytes.NewBuffer())
		_, err := startAt(s, 1, func(ni int) (launch.Handle, error) {
			tried = append(tried, s.plan.Nodes[ni].Host)
			return nil, errors.New("qrsh launch on node002: slots unavailable past slot_retry bound")
		})
		Expect(err).To(HaveOccurred())
		Expect(tried).To(Equal([]string{"node002"}))
		l, err := layout.Read(filepath.Join(dir, layout.LayoutFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Spares).To(HaveLen(1), "the spare is untouched")
	})

	It("gives a stepper that ended early one spare per step, not one per failure", func() {
		dir := writeSparesLayout(nodes, []layout.Node{{Host: "node003"}}, nil)
		stderr := gbytes.NewBuffer()
		s := elastic(stderr)
		s.softSwapped = true // an earlier stepper of this step already took one
		_, _, ok := s.replace("node002", false)
		Expect(ok).To(BeFalse())
		Expect(string(stderr.Contents())).To(ContainSubstring("ended before its tasks reported again"))
		l, err := layout.Read(filepath.Join(dir, layout.LayoutFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Spares).To(HaveLen(1))
	})

	It("takes no spare once srun is stopping the step", func() {
		dir := writeSparesLayout(nodes, []layout.Node{{Host: "node003"}}, nil)
		s := elastic(gbytes.NewBuffer())
		s.armEscalation() // its later stages find no channel to signal or close
		_, _, ok := s.replace("node002", true)
		Expect(ok).To(BeFalse())
		Expect(s.canReplace("node002", 0)).To(BeFalse(), "armEscalation marks the step as stopping [AC9]")
		l, err := layout.Read(filepath.Join(dir, layout.LayoutFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Spares).To(HaveLen(1))
	})

	It("does not register a spare's channel once srun is stopping", func() {
		s := &supervisor{}
		Expect(s.addLiveConn(&proto.Conn{})).To(BeTrue())
		s.terminating.Store(true)
		Expect(s.addLiveConn(&proto.Conn{})).To(BeFalse())
		Expect(s.allConns()).To(HaveLen(1))
	})
})

var _ = Describe("hot spares: a replacement that was replaced in turn", func() {
	It("lets a concurrent step follow the chain to the current host", func() {
		writeSparesLayout(
			[]layout.Node{{Index: 0, Host: "node001", IsMaster: true}, {Index: 1, Host: "node004"}},
			[]layout.Node{{Host: "node005"}},
			[]layout.Swap{{Lost: "node002", Spare: "node003"}, {Lost: "node003", Spare: "node004"}},
		)
		spare, _, _, err := (&supervisor{lay: &layout.Layout{}}).takeSpare("node002")
		Expect(err).NotTo(HaveOccurred())
		Expect(spare.Host).To(Equal("node004"))
	})
})

var _ = Describe("hot spares: accepting the spare's stepper", func() {
	It("skips a late stepper of an earlier spare instead of failing this one", func() {
		srv, err := proto.Listen("127.0.0.1:0", "tok")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(srv.Close)
		cfg := config.Default()
		cfg.LaunchTimeout.Duration = 5 * time.Second
		s := &supervisor{srv: srv, cfg: cfg}

		late, err := proto.Dial(srv.Addr(), "tok", "node003")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(late.Close)
		go func() {
			defer GinkgoRecover()
			time.Sleep(200 * time.Millisecond)
			c, err := proto.Dial(srv.Addr(), "tok", "node004")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(c.Close)
		}()

		c, err := s.acceptFrom("node004")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Host).To(Equal("node004"))
		_, err = late.Recv()
		Expect(err).To(HaveOccurred(), "the late stepper's channel is closed: it exits with a code")
	})
})

var _ = Describe("hot spares: swap side effects", func() {
	It("runs drain_command with the host, job and reason substituted", func() {
		r := &fake.Runner{}
		cfg := config.Default()
		cfg.DrainCommand = []string{"/opt/site/drain", "{host}", "--job={job}", "{reason}"}
		s := &supervisor{cfg: cfg, runner: r, lay: &layout.Layout{Job: layout.Job{JobID: 42}}, stderr: gbytes.NewBuffer()}
		s.drain("node002")
		Expect(r.Calls).To(HaveLen(1))
		Expect(r.Calls[0].Name).To(Equal("/opt/site/drain"))
		Expect(r.Calls[0].Args).To(Equal([]string{"node002", "--job=42", "slurm-shim: node lost during step"}))
	})

	It("warns, without failing, when drain_command fails", func() {
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 3, Stderr: []byte("boom")}
		}}
		cfg := config.Default()
		cfg.DrainCommand = []string{"drain", "{host}"}
		stderr := gbytes.NewBuffer()
		s := &supervisor{cfg: cfg, runner: r, lay: &layout.Layout{}, stderr: stderr}
		s.drain("node002")
		Expect(string(stderr.Contents())).To(ContainSubstring("drain_command for node002 failed: exit 3: boom"))
	})

	It("bounds a drain_command that hangs by drain_timeout", func() {
		cfg := config.Default()
		cfg.DrainCommand = []string{"sleep", "30"}
		cfg.DrainTimeout.Duration = 200 * time.Millisecond
		s := &supervisor{cfg: cfg, lay: &layout.Layout{}, stderr: gbytes.NewBuffer()}
		start := time.Now()
		s.drain("node002")
		Expect(time.Since(start)).To(BeNumerically("<", 5*time.Second))
	})

	It("records the swaps in the job context, warning once when that fails", func() {
		writeSparesLayout(
			[]layout.Node{{Index: 0, Host: "node001"}, {Index: 1, Host: "node004"}}, nil,
			[]layout.Swap{{Lost: "node002", Spare: "node003"}, {Lost: "node003", Spare: "node004"}},
		)
		r := &fake.Runner{Responder: func(string, []string) fake.Response {
			return fake.Response{Exit: 1, Stderr: []byte("not a submit host")}
		}}
		stderr := gbytes.NewBuffer()
		s := &supervisor{cfg: config.Default(), runner: r, remote: true, stderr: stderr}
		s.recordSwaps()
		s.recordSwaps()
		Expect(r.Calls).To(HaveLen(2))
		Expect(r.Calls[0].Name).To(Equal("qalter"))
		Expect(r.Calls[0].Args).To(Equal([]string{"-ac", "shim.swaps=node002:node003+node003:node004", "42"}))
		Expect(strings.Count(string(stderr.Contents()), "not visible to squeue/scontrol")).To(Equal(1))
	})

	It("does not record the swaps of an array task: the job context is shared by the array", func() {
		tmp := GinkgoT().TempDir()
		GinkgoT().Setenv("TMPDIR", tmp)
		task := int64(3)
		Expect(layout.Write(filepath.Join(tmp, layout.StateDir), &layout.Layout{
			SchemaVersion: layout.SchemaVersion,
			Job:           layout.Job{JobID: 42, ArrayTaskID: &task},
			Swaps:         []layout.Swap{{Lost: "node002", Spare: "node003"}},
		})).To(Succeed())
		r := &fake.Runner{}
		(&supervisor{cfg: config.Default(), runner: r, remote: true, stderr: gbytes.NewBuffer()}).recordSwaps()
		Expect(r.Calls).To(BeEmpty())
	})
})

var _ = Describe("hot spares: the job node list after a swap", func() {
	It("shows the job's current nodes to every later step", func() {
		s := &supervisor{
			cfg: config.Default(), opt: &options{},
			lay: &layout.Layout{
				Nodes: []layout.Node{{Host: "node001"}, {Host: "node003"}},
				Swaps: []layout.Swap{{Lost: "node002", Spare: "node003"}},
			},
			plan: &plan.StepPlan{NTasks: 1, Nodes: []plan.StepNode{{Host: "node001"}},
				Ranks: []plan.PlacedRank{{Rank: 0, StepNodeIndex: 0}}},
		}
		Expect(s.stepShadows()).To(ContainElements("SLURM_JOB_NODELIST=node[001,003]", "SLURM_NODELIST=node[001,003]"))
		s.lay.Swaps = nil
		Expect(s.stepShadows()).NotTo(ContainElement(HavePrefix("SLURM_JOB_NODELIST=")), "no swap: the job's own list stands")
	})
})

var _ = Describe("hot spares: torchrun command lines", func() {
	DescribeTable("finds torchrun's own options",
		func(cmd []string, want int) { Expect(torchrunOptsStart(cmd)).To(Equal(want)) },
		Entry("torchrun", []string{"torchrun", "--nnodes=2", "x.py"}, 1),
		Entry("python -m", []string{"python3", "-m", "torch.distributed.run", "x.py"}, 3),
		Entry("python flags before -m", []string{"python3", "-u", "-m", "torch.distributed.run", "x.py"}, 4),
		Entry("not torchrun", []string{"python3", "x.py"}, -1),
	)

	DescribeTable("accepts an elastic --nnodes range that covers the step",
		func(nnodes string, want bool) { Expect(nnodesCovers(nnodes, 2)).To(Equal(want)) },
		Entry("exact", "2", true),
		Entry("range", "1:4", true),
		Entry("too few", "1", false),
		Entry("range above", "3:4", false),
		Entry("garbage", "x", false),
	)
})

// startAt starts step node ni as startSteppers does for one node: the start,
// then the move to spares while the host refuses the task.
func startAt(s *supervisor, ni int, start func(ni int) (launch.Handle, error)) (launch.Handle, error) {
	h, err := start(ni)
	if err == nil {
		return h, nil
	}
	return s.startOnSpare(ni, err, start)
}
