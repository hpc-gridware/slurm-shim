package install_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/install"
)

const prefix = "/opt/ocs/slurm-shim"

// bare is a stock cluster: all.q with the default PEs, none of them ours.
func bare() *fakeAdmin {
	f := newFake()
	f.pes["make"] = gedata.PE{Name: "make", Slots: 999, StartProcArgs: "/opt/mpi/start.sh", AllocationRule: "$round_robin"}
	f.queues["all.q"] = gedata.Queue{Name: "all.q", PEList: []string{"make"}, ShellStartMode: "unix_behavior"}
	f.hosts = []string{"h1", "h2"}
	return f
}

func kinds(p install.Plan) []install.ChangeKind {
	var out []install.ChangeKind
	for _, c := range p.Changes {
		out = append(out, c.Kind)
	}
	return out
}

func find(p install.Plan, kind install.ChangeKind, object string) (install.Change, bool) {
	for _, c := range p.Changes {
		if c.Kind == kind && c.Object == object {
			return c, true
		}
	}
	return install.Change{}, false
}

var _ = Describe("MakePlan", func() {
	ctx := context.Background()

	It("creates a dedicated PE with the reference shape and wires the queue", func() {
		f := bare()
		facts, err := install.Discover(ctx, f)
		Expect(err).NotTo(HaveOccurred())
		p := install.MakePlan(facts, install.Options{Prefix: prefix})

		Expect(kinds(p)).To(ContainElements(install.ChangeAddPE, install.ChangeAddToPEList, install.ChangeSetStarter))
		Expect(p.PE.Name).To(Equal("slurm-shim"))
		Expect(p.PE.ControlSlaves).To(BeTrue(), "qrsh -inherit needs control_slaves TRUE")
		Expect(p.PE.StartProcArgs).To(Equal(prefix + "/bin/slurm-shim-env"))
		Expect(p.PE.AllocationRule).To(Equal("$round_robin"))
		Expect(p.PE.Slots).To(Equal(999))
		Expect(p.PE.JobIsFirstTask).To(BeFalse())
		Expect(p.PE.DaemonForksSlaves).To(BeFalse())

		s, ok := find(p, install.ChangeSetStarter, "all.q")
		Expect(ok).To(BeTrue())
		Expect(s.New).To(Equal(prefix + "/bin/slurm-shim-starter"))
		Expect(p.Mutating()).To(BeTrue())
		Expect(p.Refusals()).To(BeEmpty())
	})

	It("never touches an existing PE's start_proc_args without --force (MPI safety)", func() {
		f := bare()
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix, PEName: "make"})

		r, ok := find(p, install.ChangeRefused, "make")
		Expect(ok).To(BeTrue(), "an existing PE with foreign start_proc_args must be refused")
		Expect(r.Attr).To(Equal("start_proc_args"))
		Expect(r.Old).To(Equal("/opt/mpi/start.sh"))
		Expect(r.Reason).To(ContainSubstring("MPI"))
		Expect(kinds(p)).NotTo(ContainElement(install.ChangeSetPEAttr))
		Expect(kinds(p)).NotTo(ContainElement(install.ChangeAddPE))
	})

	It("repairs an existing PE only with --force, attribute by attribute", func() {
		f := bare()
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix, PEName: "make", Force: true})

		fix, ok := find(p, install.ChangeSetPEAttr, "make")
		Expect(ok).To(BeTrue())
		Expect(fix.Attr).To(Equal("start_proc_args"))
		Expect(fix.New).To(Equal(prefix + "/bin/slurm-shim-env"))
		cs, ok := findAttr(p, install.ChangeSetPEAttr, "make", "control_slaves")
		Expect(ok).To(BeTrue(), "make has control_slaves FALSE and must be repaired")
		Expect(cs.New).To(Equal("TRUE"))
		Expect(p.Refusals()).To(BeEmpty())
	})

	It("refuses to replace a queue's existing starter_method without --force", func() {
		f := bare()
		q := f.queues["all.q"]
		q.StarterMethod = "/site/starter.sh"
		f.queues["all.q"] = q
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix})

		r, ok := find(p, install.ChangeRefused, "all.q")
		Expect(ok).To(BeTrue())
		Expect(r.Attr).To(Equal("starter_method"))
		Expect(r.Old).To(Equal("/site/starter.sh"))
		Expect(kinds(p)).NotTo(ContainElement(install.ChangeSetStarter))

		forced := install.MakePlan(facts, install.Options{Prefix: prefix, Force: true})
		s, ok := find(forced, install.ChangeSetStarter, "all.q")
		Expect(ok).To(BeTrue())
		Expect(s.Old).To(Equal("/site/starter.sh"))
	})

	It("is a no-op on an already-configured cluster (idempotency)", func() {
		f := bare()
		f.pes["slurm-shim"] = install.ReferencePE("slurm-shim", prefix)
		q := f.queues["all.q"]
		q.PEList = append(q.PEList, "slurm-shim")
		q.StarterMethod = install.StarterPath(prefix)
		f.queues["all.q"] = q
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix})

		Expect(p.Mutating()).To(BeFalse())
		Expect(p.Refusals()).To(BeEmpty())
		for _, c := range p.Changes {
			Expect(c.Kind).To(Equal(install.ChangeUnchanged))
		}
	})

	It("derives partitions from queues and prefers all.q as the default", func() {
		f := bare()
		f.queues["gpu.q"] = gedata.Queue{Name: "gpu.q"}
		f.queues["debug.q"] = gedata.Queue{Name: "debug.q"}
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix})

		names := map[string]string{}
		for _, part := range p.Partitions {
			names[part.Name] = part.Queue
		}
		Expect(names).To(Equal(map[string]string{"all": "all.q", "gpu": "gpu.q", "debug": "debug.q"}))
		Expect(p.DefaultPartition).To(Equal("all"))
	})

	It("falls back to the first queue when there is no all.q", func() {
		f := newFake()
		f.queues["gpu.q"] = gedata.Queue{Name: "gpu.q"}
		f.queues["batch.q"] = gedata.Queue{Name: "batch.q"}
		facts, _ := install.Discover(ctx, f)
		Expect(install.MakePlan(facts, install.Options{Prefix: prefix}).DefaultPartition).To(Equal("batch"))
	})

	It("limits wiring to --queue when given", func() {
		f := bare()
		f.queues["gpu.q"] = gedata.Queue{Name: "gpu.q"}
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix, Queues: []string{"gpu.q"}})
		_, all := find(p, install.ChangeSetStarter, "all.q")
		_, gpu := find(p, install.ChangeSetStarter, "gpu.q")
		Expect(all).To(BeFalse())
		Expect(gpu).To(BeTrue())
		Expect(p.Partitions).To(HaveLen(1))
	})

	It("detects an RSMAP complex as the GPU complex", func() {
		f := bare()
		f.complexes = []gedata.Complex{{Name: "mem_free", Type: "MEMORY"}, {Name: "gpu", Type: "RSMAP", Consumable: "HOST"}}
		facts, _ := install.Discover(ctx, f)
		Expect(install.MakePlan(facts, install.Options{Prefix: prefix}).GPUComplex).To(Equal("gpu"))
	})
})

func findAttr(p install.Plan, kind install.ChangeKind, object, attr string) (install.Change, bool) {
	for _, c := range p.Changes {
		if c.Kind == kind && c.Object == object && c.Attr == attr {
			return c, true
		}
	}
	return install.Change{}, false
}
