package install_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/install"
)

var _ = Describe("GenerateConfig", func() {
	ctx := context.Background()

	planFor := func(f *fakeAdmin) install.Plan {
		facts, _ := install.Discover(ctx, f)
		return install.MakePlan(facts, install.Options{Prefix: prefix})
	}

	It("starts from the compiled defaults, so the safe memory complex and port range come along", func() {
		cfg := install.GenerateConfig(planFor(bare()), nil)
		Expect(cfg.MemoryComplex).To(Equal("mem_free"))
		Expect(cfg.ControlPortBase).To(Equal(61000))
		Expect(cfg.DefaultPartition).To(Equal("all"))
		Expect(cfg.Partitions["all"]).To(Equal(config.Partition{Queue: "all.q", PE: "slurm-shim", Slots: "per-task"}))
		Expect(cfg.PEs["slurm-shim"].TaskPolicy).To(Equal("slot"))
	})

	It("merges into an existing config: never rewrites a site partition or its default", func() {
		existing := config.Default()
		existing.DefaultPartition = "gpu"
		existing.Partitions = map[string]config.Partition{"all": {Queue: "all.q", PE: "mpi", Slots: "16"}}
		existing.MemoryComplex = "h_vmem"

		f := bare()
		f.queues["gpu.q"] = gedata.Queue{Name: "gpu.q"}
		cfg := install.GenerateConfig(planFor(f), existing)

		Expect(cfg.Partitions["all"]).To(Equal(config.Partition{Queue: "all.q", PE: "mpi", Slots: "16"}), "site's partition untouched")
		Expect(cfg.Partitions["gpu"].PE).To(Equal("slurm-shim"), "missing partition added")
		Expect(cfg.DefaultPartition).To(Equal("gpu"), "site's default kept")
		Expect(cfg.MemoryComplex).To(Equal("h_vmem"), "site's other settings untouched (the plan warns, it does not overwrite)")
	})

	It("adopts a discovered RSMAP name only when the site left the default", func() {
		f := bare()
		f.complexes = []gedata.Complex{{Name: "nvidia", Type: "RSMAP"}}
		Expect(install.GenerateConfig(planFor(f), nil).GPU.GresComplex).To(Equal("nvidia"))

		site := config.Default()
		site.GPU.GresComplex = "mygpu"
		Expect(install.GenerateConfig(planFor(f), site).GPU.GresComplex).To(Equal("mygpu"))
	})

	It("renders and parses back identically (Duration round-trips)", func() {
		cfg := install.GenerateConfig(planFor(bare()), nil)
		data, err := config.Render(cfg)
		Expect(err).NotTo(HaveOccurred())
		back, warns, err := config.Parse(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(warns).To(BeEmpty())
		Expect(back.QstatTimeout).To(Equal(cfg.QstatTimeout))
		Expect(back.Partitions).To(Equal(cfg.Partitions))
	})
})
