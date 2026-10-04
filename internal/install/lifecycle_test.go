package install_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/install"
)

// uninstallOpts is PlanUninstall for the tree at prefix, matched lexically.
var uninstallOpts = install.UninstallOptions{InTree: install.PathIn(prefix)}

// installed applies a plan to f and returns the record install would write.
func installed(f *fakeAdmin, o install.Options) *install.State {
	facts, err := install.Discover(context.Background(), f)
	Expect(err).NotTo(HaveOccurred())
	p := install.MakePlan(facts, o)
	st := &install.State{Prefix: prefix}
	st.Intend(p)
	st.Record(install.Apply(context.Background(), f, p))
	return st
}

func uninstallPlan(f *fakeAdmin, st *install.State, o install.UninstallOptions) install.Plan {
	facts, err := install.Discover(context.Background(), f)
	Expect(err).NotTo(HaveOccurred())
	return install.PlanUninstall(facts, st, o)
}

var _ = Describe("MakePlan: which queues a first install touches", func() {
	ctx := context.Background()

	It("creates slurm.q from all.q, entered only through the FORCED complex, and changes no existing queue", func() {
		f := bare()
		facts, err := install.Discover(ctx, f)
		Expect(err).NotTo(HaveOccurred())
		p := install.MakePlan(facts, install.Options{Prefix: prefix})

		_, ok := find(p, install.ChangeAddComplex, install.ShimComplex)
		Expect(ok).To(BeTrue(), "the complex comes first, the queue references it")
		add, ok := find(p, install.ChangeAddQueue, "slurm.q")
		Expect(ok).To(BeTrue())
		Expect(add.Old).To(Equal("all.q"), "cloned from all.q")
		Expect(add.New).To(Equal("slurm_shim=TRUE"))
		for _, c := range p.Changes {
			Expect(c.Object).NotTo(Equal("all.q"), "all.q is not touched: %+v", c)
		}
		Expect(p.Partitions).To(Equal([]install.Partition{{Name: "slurm", Queue: "slurm.q", Request: "slurm_shim=TRUE"}}))
		Expect(p.DefaultPartition).To(Equal("slurm"))
		Expect(install.GenerateConfig(p, nil).Partitions["slurm"].Request).To(Equal("slurm_shim=TRUE"),
			"sbatch and srun request the complex for this partition")

		install.Apply(ctx, f, p)
		Expect(f.calls).To(ContainElement("CloneQueue all.q -> slurm.q complex_values=slurm_shim=TRUE"))
		Expect(f.queues["slurm.q"].PEList).To(Equal([]string{"slurm-shim"}))
		Expect(f.queues["slurm.q"].StarterMethod).To(Equal(prefix + "/bin/slurm-shim-starter"))
		Expect(f.queues["all.q"]).To(Equal(bare().queues["all.q"]))
	})

	It("keeps the queues an earlier install wired and creates nothing on a re-run", func() {
		f := bare()
		installed(f, install.Options{Prefix: prefix})
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix})
		Expect(p.Mutating()).To(BeFalse())
		Expect(p.Partitions).To(Equal([]install.Partition{{Name: "slurm", Queue: "slurm.q", Request: "slurm_shim=TRUE"}}))
	})

	It("wires every queue with --queue all, and refuses it when a queue is literally named all", func() {
		f := bare()
		f.queues["gpu.q"] = gedata.Queue{Name: "gpu.q"}
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix, Queues: []string{"all"}})
		Expect(kinds(p)).NotTo(ContainElements(install.ChangeAddQueue, install.ChangeAddComplex))
		Expect(p.Partitions).To(HaveLen(2))
		Expect(p.Partitions[0].Request).To(BeEmpty(), "existing queues need no request")
		Expect(install.AmbiguousAllQueues(facts, []string{"all"})).To(BeFalse())

		f.queues["all"] = gedata.Queue{Name: "all"}
		facts, _ = install.Discover(ctx, f)
		Expect(install.AmbiguousAllQueues(facts, []string{"all"})).To(BeTrue())
	})

	It("refuses an unknown --queue name and all combined with named queues, instead of skipping them", func() {
		facts, _ := install.Discover(ctx, bare())
		p := install.MakePlan(facts, install.Options{Prefix: prefix, Queues: []string{"all.q", "gpuq"}})
		Expect(p.Refusals()).To(ConsistOf(HaveField("Object", "gpuq")))
		p = install.MakePlan(facts, install.Options{Prefix: prefix, Queues: []string{"all", "all.q"}})
		Expect(p.Refusals()).To(ConsistOf(HaveField("Object", "all")))
		Expect(p.Partitions).To(BeEmpty())
	})

	It("refuses to adopt a slurm.q it did not create, but finishes one its record names", func() {
		f := bare()
		f.queues["slurm.q"] = gedata.Queue{Name: "slurm.q"}
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix})
		Expect(p.Refusals()).To(ContainElement(HaveField("Object", "slurm.q")))
		Expect(kinds(p)).NotTo(ContainElement(install.ChangeAddQueue))

		// An interrupted first install: the queue was created, the starter not set.
		p = install.MakePlan(facts, install.Options{Prefix: prefix, CreatedQueue: "slurm.q"})
		Expect(p.Refusals()).To(BeEmpty())
		_, ok := find(p, install.ChangeSetStarter, "slurm.q")
		Expect(ok).To(BeTrue(), "the re-run wires it")
	})

	It("uses an existing FORCED slurm_shim complex and refuses one with another meaning", func() {
		f := bare()
		f.complexes = []gedata.Complex{{Name: "slurm_shim", Type: "BOOL", Requestable: "FORCED"}}
		facts, _ := install.Discover(ctx, f)
		p := install.MakePlan(facts, install.Options{Prefix: prefix})
		Expect(kinds(p)).To(ContainElement(install.ChangeAddQueue))
		Expect(kinds(p)).NotTo(ContainElement(install.ChangeAddComplex))

		f.complexes = []gedata.Complex{{Name: "slurm_shim", Type: "INT", Requestable: "YES"}}
		facts, _ = install.Discover(ctx, f)
		p = install.MakePlan(facts, install.Options{Prefix: prefix})
		Expect(p.Refusals()).To(ContainElement(HaveField("Object", "slurm_shim")))
		Expect(kinds(p)).NotTo(ContainElement(install.ChangeAddQueue))
	})
})

var _ = Describe("CloneWarnings", func() {
	ctx := context.Background()
	insts := []gedata.QueueInstance{
		{Queue: "all.q", Host: "h1", States: ""},
		{Queue: "all.q", Host: "h2", States: "d"},
		{Queue: "other.q", Host: "h3"},
	}

	It("names what slurm.q does not inherit: disabled hosts, slots limits, subordination, quotas", func() {
		f := bare()
		f.unlimited = map[string]bool{"h1": true, "h3": true}
		f.queues["low.q"] = gedata.Queue{Name: "low.q"}
		f.queues["prio.q"] = gedata.Queue{Name: "prio.q", Subordinates: []string{"all.q=1"}}
		f.rqs = []gedata.RQS{{Name: "peruser", Enabled: true, Limits: []string{"users {*} queues all.q to slots=10"}}}
		facts, _ := install.Discover(ctx, f)
		w := install.CloneWarnings(ctx, f, facts, "all.q", insts)
		Expect(w).To(ContainElement(ContainSubstring("disabled or in error: h2 (d)")))
		Expect(w).To(ContainElement(ContainSubstring("1 exec host(s) without a slots limit: h1 ")),
			"only all.q's hosts; h3 does not get slurm.q")
		Expect(w).To(ContainElement(ContainSubstring("queue prio.q suspends all.q")))
		Expect(w).To(ContainElement(ContainSubstring("resource quota peruser names all.q")))
		for _, c := range f.calls {
			Expect(c).NotTo(ContainSubstring("exechost"), "hosts are never changed")
		}
	})

	It("takes a resource quota on slots per host as the hosts' limit", func() {
		f := bare()
		f.unlimited = map[string]bool{"h1": true}
		f.rqs = []gedata.RQS{{Name: "cores", Enabled: true, Limits: []string{"hosts {*} to slots=$num_proc"}}}
		facts, _ := install.Discover(ctx, f)
		Expect(install.CloneWarnings(ctx, f, facts, "all.q", insts)).NotTo(ContainElement(ContainSubstring("slots limit")))
	})

	It("says so when there is no all.q to clone", func() {
		f := bare()
		facts, _ := install.Discover(ctx, f)
		Expect(install.CloneWarnings(ctx, f, facts, "", nil)).To(ConsistOf(ContainSubstring("1 slot per host")))
	})
})

var _ = Describe("the install record", func() {
	It("records what install is about to create before applying, and what it replaced after", func() {
		f := bare()
		q := f.queues["all.q"]
		q.StarterMethod = "/site/starter.sh"
		f.queues["all.q"] = q
		st := installed(f, install.Options{Prefix: prefix, Queues: all, Force: true})
		Expect(st.CreatedPE).To(Equal("slurm-shim"))
		Expect(st.PEListAdds).To(ContainElement(install.PEListAdd{Queue: "all.q", PE: "slurm-shim"}))
		Expect(st.Replaced).To(ContainElement(install.Replaced{Object: "queue all.q", Attr: "starter_method", Old: "/site/starter.sh"}))

		facts, _ := install.Discover(context.Background(), bare())
		intent := &install.State{}
		intent.Intend(install.MakePlan(facts, install.Options{Prefix: prefix}))
		Expect(intent.CreatedQueue).To(Equal("slurm.q"))
		Expect(intent.CreatedComplex).To(Equal("slurm_shim"))
	})

	It("round-trips, and is refused when a symlink, writable by others, or from a newer installer", func() {
		dir := GinkgoT().TempDir()
		Expect(install.WriteState(dir, &install.State{Prefix: dir, CreatedQueue: "slurm.q"})).To(Succeed())
		st, err := install.ReadState(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.CreatedQueue).To(Equal("slurm.q"))

		path := filepath.Join(dir, install.StateRel)
		Expect(os.Chmod(path, 0o666)).To(Succeed())
		_, err = install.ReadState(dir)
		Expect(err).To(MatchError(ContainSubstring("writable by group or others")))

		Expect(os.Remove(path)).To(Succeed())
		other := filepath.Join(GinkgoT().TempDir(), "elsewhere.json")
		Expect(os.WriteFile(other, []byte(`{"created_queue":"all.q"}`), 0o644)).To(Succeed())
		Expect(os.Symlink(other, path)).To(Succeed())
		_, err = install.ReadState(dir)
		Expect(err).To(MatchError(ContainSubstring("not a regular file")))
		Expect(install.WriteState(dir, &install.State{})).To(Succeed(), "a write replaces the link")
		Expect(os.ReadFile(other)).To(ContainSubstring("all.q"), "and never writes through it")

		Expect(os.WriteFile(path, []byte(`{"schema_version":2}`), 0o644)).To(Succeed())
		_, err = install.ReadState(dir)
		Expect(err).To(MatchError(ContainSubstring("newer")))

		missing, err := install.ReadState(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		Expect(missing).To(BeNil())
	})
})

var _ = Describe("PlanUninstall", func() {
	ctx := context.Background()

	snapshot := func(f *fakeAdmin) (map[string]gedata.Queue, map[string]gedata.PE, []gedata.Complex) {
		qs, ps := map[string]gedata.Queue{}, map[string]gedata.PE{}
		for k, v := range f.queues {
			qs[k] = v
		}
		for k, v := range f.pes {
			ps[k] = v
		}
		return qs, ps, append([]gedata.Complex(nil), f.complexes...)
	}

	It("undoes a default install exactly: slurm.q, the PE and the complex are gone, all.q as before", func() {
		f := bare()
		beforeQ, beforeP, beforeC := snapshot(f)
		st := installed(f, install.Options{Prefix: prefix})
		Expect(st.CreatedQueue).To(Equal("slurm.q"))

		u := uninstallPlan(f, st, uninstallOpts)
		Expect(u.Refusals()).To(BeEmpty())
		Expect(install.Apply(ctx, f, u).Failed()).To(BeEmpty())
		afterQ, afterP, afterC := snapshot(f)
		Expect(reflect.DeepEqual(afterQ, beforeQ)).To(BeTrue(), "queues: %v", afterQ)
		Expect(reflect.DeepEqual(afterP, beforeP)).To(BeTrue(), "pes: %v", afterP)
		Expect(afterC).To(Equal(beforeC))

		facts, _ := install.Discover(ctx, f)
		Expect(install.References(facts, install.PathIn(prefix))).To(BeEmpty())
		Expect(install.PlanUninstall(facts, st, uninstallOpts).Mutating()).To(BeFalse(), "a second uninstall has nothing to do")
	})

	It("restores what install --force replaced, from the record", func() {
		f := bare()
		q := f.queues["all.q"]
		q.StarterMethod = "/site/starter.sh"
		f.queues["all.q"] = q
		st := installed(f, install.Options{Prefix: prefix, Queues: all, Force: true})
		install.Apply(ctx, f, uninstallPlan(f, st, uninstallOpts))
		Expect(f.queues["all.q"].StarterMethod).To(Equal("/site/starter.sh"))
		Expect(f.queues["all.q"].PEList).To(Equal([]string{"make"}))
	})

	It("restores every attribute of a site PE taken over with --pe NAME --force, and the queues it was added to", func() {
		f := bare()
		f.queues["gpu.q"] = gedata.Queue{Name: "gpu.q"}
		before := f.pes["make"]
		st := installed(f, install.Options{Prefix: prefix, PEName: "make", Queues: all, Force: true})
		Expect(f.pes["make"].ControlSlaves).To(BeTrue())

		u := uninstallPlan(f, st, uninstallOpts)
		Expect(kinds(u)).NotTo(ContainElement(install.ChangeDeletePE), "a site PE is never deleted")
		install.Apply(ctx, f, u)
		Expect(f.pes["make"].StartProcArgs).To(Equal(before.StartProcArgs))
		Expect(f.pes["make"].ControlSlaves).To(BeFalse())
		Expect(f.queues["gpu.q"].PEList).To(BeEmpty(), "install added make to gpu.q")
		Expect(f.queues["all.q"].PEList).To(Equal([]string{"make"}), "all.q had it before install")
	})

	It("restores, never deletes, the default PE when this install only took it over from another", func() {
		f := bare()
		f.pes["slurm-shim"] = install.ReferencePE("slurm-shim", "/opt/other")
		st := installed(f, install.Options{Prefix: prefix, Queues: all, Force: true})
		u := uninstallPlan(f, st, uninstallOpts)
		Expect(kinds(u)).NotTo(ContainElement(install.ChangeDeletePE))
		install.Apply(ctx, f, u)
		Expect(f.pes["slurm-shim"].StartProcArgs).To(Equal("/opt/other/bin/slurm-shim-env"))
	})

	It("without a record unwires by what points at the tree, and keeps a slurm.q it cannot prove it created", func() {
		f := bare()
		installed(f, install.Options{Prefix: prefix})
		u := uninstallPlan(f, nil, uninstallOpts)
		Expect(kinds(u)).NotTo(ContainElements(install.ChangeDeleteQueue, install.ChangeDeleteComplex))
		Expect(u.Warnings).To(ContainElement(ContainSubstring("slurm.q is unwired but kept")))
		install.Apply(ctx, f, u)
		Expect(f.queues["slurm.q"].PEList).To(BeEmpty())
		Expect(f.queues["slurm.q"].StarterMethod).To(BeEmpty())
		Expect(f.pes).NotTo(HaveKey("slurm-shim"))
	})

	It("never deletes a queue on the record's word alone", func() {
		f := bare()
		installed(f, install.Options{Prefix: prefix, Queues: all})
		u := uninstallPlan(f, &install.State{CreatedQueue: "all.q"}, uninstallOpts)
		Expect(kinds(u)).NotTo(ContainElement(install.ChangeDeleteQueue))

		f = bare()
		f.queues["slurm.q"] = gedata.Queue{Name: "slurm.q", PEList: []string{"make"}} // the site's own, later
		u = uninstallPlan(f, &install.State{CreatedQueue: "slurm.q"}, uninstallOpts)
		Expect(kinds(u)).NotTo(ContainElement(install.ChangeDeleteQueue))
		Expect(u.Warnings).To(ContainElement(ContainSubstring("slurm.q is kept")))
	})

	It("needs --pe for a custom-named PE without a record, and refuses to guess", func() {
		f := bare()
		installed(f, install.Options{Prefix: prefix, PEName: "mpi-shim", Queues: all})
		u := uninstallPlan(f, nil, uninstallOpts)
		Expect(u.Refusals()).To(ContainElement(HaveField("Reason", ContainSubstring("--pe mpi-shim"))))

		u = uninstallPlan(f, nil, install.UninstallOptions{InTree: install.PathIn(prefix), PEName: "mpi-shim"})
		Expect(u.Refusals()).To(BeEmpty())
		install.Apply(ctx, f, u)
		Expect(f.pes).NotTo(HaveKey("mpi-shim"))
		Expect(f.queues["all.q"].PEList).To(Equal([]string{"make"}))
	})

	It("refuses a recorded value that runs as another user, and per-host entries it cannot remove", func() {
		f := bare()
		pe := f.pes["make"]
		pe.StartProcArgs = prefix + "/bin/slurm-shim-env"
		f.pes["make"] = pe
		st := &install.State{Replaced: []install.Replaced{{Object: "pe make", Attr: "start_proc_args", Old: "root@/tmp/x"}}}
		Expect(uninstallPlan(f, st, uninstallOpts).Refusals()).To(ContainElement(HaveField("Reason", ContainSubstring("another user"))))

		f = bare()
		installed(f, install.Options{Prefix: prefix, Queues: all})
		q := f.queues["all.q"]
		q.PEListOverrides = []string{"[@gpu=make slurm-shim]"}
		f.queues["all.q"] = q
		Expect(uninstallPlan(f, nil, uninstallOpts).Refusals()).To(ContainElement(HaveField("Reason", ContainSubstring("per-host"))))
	})

	It("recognises the tree under another spelling (a symlink to it)", func() {
		real := filepath.Join(GinkgoT().TempDir(), "shim")
		Expect(os.MkdirAll(filepath.Join(real, "bin"), 0o755)).To(Succeed())
		link := filepath.Join(GinkgoT().TempDir(), "link")
		Expect(os.Symlink(real, link)).To(Succeed())

		f := bare()
		installed(f, install.Options{Prefix: link, Queues: all})
		u := uninstallPlan(f, nil, install.UninstallOptions{InTree: install.TreeMatcher(real)})
		_, ok := find(u, install.ChangeSetStarter, "all.q")
		Expect(ok).To(BeTrue(), "the starter under the link is this tree")
		Expect(uninstallPlan(f, nil, install.UninstallOptions{InTree: install.TreeMatcher("/opt/other")}).Changes).To(BeEmpty())
	})
})

var _ = Describe("BusyJobs", func() {
	It("names jobs, pending or running, that use the PE, queue or complex uninstall deletes", func() {
		f := bare()
		st := installed(f, install.Options{Prefix: prefix})
		u := uninstallPlan(f, st, uninstallOpts)
		busy := install.BusyJobs(u, []gedata.JobUse{
			{ID: "10", PE: "slurm-shim"},            // pending, by name
			{ID: "11", PE: "slurm*"},                // pending, by wildcard
			{ID: "12", Queues: []string{"slurm.q"}}, // hard queue request
			{ID: "13", Resources: []string{"slurm_shim"}},
			{ID: "14", PE: "make", Queues: []string{"all.q"}},
		})
		Expect(busy).To(Equal([]string{"10", "11", "12", "13"}))
	})
})

var _ = Describe("removing the tree", func() {
	tree := func() string {
		pfx := filepath.Join(GinkgoT().TempDir(), "shim")
		for _, rel := range []string{"bin/slurm-shim", "bin/slurm-shim-starter", "etc/slurm-shim-source-hook.sh",
			"etc/install-state.json", "etc/site-notes.txt"} {
			Expect(os.MkdirAll(filepath.Dir(filepath.Join(pfx, rel)), 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(pfx, rel), []byte("x"), 0o644)).To(Succeed())
		}
		Expect(os.Symlink("slurm-shim", filepath.Join(pfx, "bin", "sbatch"))).To(Succeed())
		_, err := install.Expose(pfx, install.ExposeModule, "0.7.1")
		Expect(err).NotTo(HaveOccurred())
		return pfx
	}

	It("removes the payload and empty directories, never a file it did not install", func() {
		pfx := tree()
		profileD := filepath.Join(GinkgoT().TempDir(), "slurm-shim.sh")
		Expect(os.WriteFile(profileD, []byte("# slurm-shim: SLURM commands for Open Cluster Scheduler\nexport PATH="+pfx+"/bin:$PATH\n"), 0o644)).To(Succeed())

		Expect(install.IsShimTree(pfx)).To(Succeed())
		_, err := install.RemoveTree(pfx, profileD, []string{pfx})
		Expect(err).NotTo(HaveOccurred())
		Expect(filepath.Join(pfx, "bin")).NotTo(BeADirectory())
		Expect(filepath.Join(pfx, "share")).NotTo(BeADirectory())
		Expect(filepath.Join(pfx, "etc", "site-notes.txt")).To(BeARegularFile(), "a foreign file stays, and so does its directory")
		Expect(profileD).NotTo(BeAnExistingFile())
	})

	It("leaves a profile.d file that is not exactly this tree's", func() {
		pfx := tree()
		profileD := filepath.Join(GinkgoT().TempDir(), "slurm-shim.sh")
		Expect(os.WriteFile(profileD, []byte("export PATH=/x"+pfx+"/bin:$PATH\n"), 0o644)).To(Succeed())
		_, err := install.RemoveTree(pfx, profileD, []string{pfx})
		Expect(err).NotTo(HaveOccurred())
		Expect(profileD).To(BeAnExistingFile())
	})

	It("is not a shim tree without bin/slurm-shim, and never removes a command that is not a link to it", func() {
		usr := GinkgoT().TempDir()
		Expect(os.MkdirAll(filepath.Join(usr, "bin"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(usr, "bin", "sbatch"), []byte("real slurm"), 0o755)).To(Succeed())
		Expect(install.IsShimTree(usr)).To(MatchError(ContainSubstring("not a slurm-shim install tree")))

		pfx := tree()
		Expect(os.Remove(filepath.Join(pfx, "bin", "sbatch"))).To(Succeed())
		Expect(os.WriteFile(filepath.Join(pfx, "bin", "sbatch"), []byte("real slurm"), 0o755)).To(Succeed())
		files, err := install.TreeFiles(pfx)
		Expect(err).NotTo(HaveOccurred())
		Expect(files).NotTo(ContainElement("bin/sbatch"))
		_, err = install.RemoveTree(pfx, "", []string{pfx})
		Expect(err).NotTo(HaveOccurred())
		Expect(filepath.Join(pfx, "bin", "sbatch")).To(BeARegularFile())
	})

	It("does not follow a symlinked directory out of the tree", func() {
		pfx := tree()
		outside := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(outside, "precious"), []byte("## slurm-shim: not really"), 0o644)).To(Succeed())
		Expect(os.RemoveAll(filepath.Join(pfx, "share"))).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(pfx, "share", "modulefiles"), 0o755)).To(Succeed())
		Expect(os.Symlink(outside, filepath.Join(pfx, "share", "modulefiles", "slurm-shim"))).To(Succeed())
		_, err := install.RemoveTree(pfx, "", []string{pfx})
		Expect(err).NotTo(HaveOccurred())
		Expect(filepath.Join(outside, "precious")).To(BeARegularFile())
	})
})
