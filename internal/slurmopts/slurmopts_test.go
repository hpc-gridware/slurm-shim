package slurmopts_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/slurmopts"
)

var _ = Describe("ParseManPage", func() {
	It("reads every heading shape SchedMD uses", func() {
		page := `<DL COMPACT>
<dt><B>-A</B>, <B>--account</B>=&lt;<I>account</I>&gt;<a class="slurm_link" id="OPT_account" href="#OPT_account"></a></dt><dd>x
<DT><DD>
<dt><B>--exclusive</B>[={user|mcs|topo}]<a class="slurm_link" id="OPT_exclusive" href="#OPT_exclusive"></a></dt><dd>x
<dt><B>-W</B>, <B>--wait</B><a class="slurm_link" id="OPT_wait" href="#OPT_wait"></a></dt><dd>x
<dt><B>--json</B>, <B>--json</B>=<I>list</I>, <B>--json</B>=&lt;<I>data_parser</I>&gt;<a class="slurm_link" id="OPT_json" href="#OPT_json"></a></dt><dd>x
<dt><B>--units</B>=[<B>KMGTP</B>]<a class="slurm_link" id="OPT_units" href="#OPT_units"></a></dt><dd>x
<dt><B>-V, --version</B><a class="slurm_link" id="OPT_version" href="#OPT_version"></a></dt><dd>x
<dt><B>-w</B>, <B>--nodelist=</B>host1,host2,...<a class="slurm_link" id="OPT_nodelist=" href="#OPT_nodelist="></a></dt><dd>x
<dt><B>SLURM_JOB_ID</B><a class="slurm_link" id="OPT_SLURM_JOB_ID" href="#OPT_SLURM_JOB_ID"></a></dt><dd>an environment variable, not an option
</DL>`
		Expect(slurmopts.ParseManPage(page)).To(Equal([]slurmopts.Option{
			{Names: []string{"-A", "--account"}, Value: slurmopts.Required},
			{Names: []string{"--exclusive"}, Value: slurmopts.Optional},
			{Names: []string{"-W", "--wait"}, Value: slurmopts.None},
			{Names: []string{"--json"}, Value: slurmopts.Optional},
			{Names: []string{"--units"}, Value: slurmopts.Required},
			{Names: []string{"-V", "--version"}, Value: slurmopts.None},
			{Names: []string{"-w", "--nodelist"}, Value: slurmopts.Required},
		}))
	})

	It("round-trips through the fixture format", func() {
		opts := []slurmopts.Option{{Names: []string{"-A", "--account"}, Value: slurmopts.Required}}
		back, err := slurmopts.ParseTable(slurmopts.FormatTable(opts))
		Expect(err).NotTo(HaveOccurred())
		Expect(back).To(Equal(opts))
		_, err = slurmopts.ParseTable("--x sometimes\n")
		Expect(err).To(MatchError(ContainSubstring("unknown kind")))
	})
})

var _ = Describe("the embedded SLURM 24.05 tables", func() {
	It("has a table for every emulated command", func() {
		for _, c := range slurmopts.Commands {
			Expect(slurmopts.For(c).Options()).NotTo(BeEmpty(), c)
		}
	})

	DescribeTable("knows the arity of the options the shim must step over",
		func(cmd, tok string, want slurmopts.Kind) {
			o, ok := slurmopts.For(cmd).Lookup(tok)
			Expect(ok).To(BeTrue(), "%s %s", cmd, tok)
			Expect(o.Value).To(Equal(want), "%s %s", cmd, tok)
		},
		Entry(nil, "sbatch", "--qos", slurmopts.Required),
		Entry(nil, "sbatch", "-q", slurmopts.Required),
		Entry(nil, "sbatch", "-C", slurmopts.Required),
		Entry(nil, "sbatch", "--licenses", slurmopts.Required),
		Entry(nil, "sbatch", "--wait", slurmopts.None),
		Entry(nil, "sbatch", "-H", slurmopts.None),
		Entry(nil, "sbatch", "--exclusive", slurmopts.Optional),
		Entry(nil, "srun", "-u", slurmopts.None),
		Entry(nil, "srun", "-I", slurmopts.Optional),
		Entry(nil, "srun", "--multi-prog", slurmopts.None),
		Entry(nil, "srun", "-x", slurmopts.Required),
		Entry(nil, "scancel", "-t", slurmopts.Required),
		Entry(nil, "scancel", "--name", slurmopts.Required),
		Entry(nil, "scancel", "--me", slurmopts.None),
		Entry(nil, "squeue", "-t", slurmopts.Required),
		Entry(nil, "squeue", "--me", slurmopts.None),
		Entry(nil, "squeue", "--json", slurmopts.Optional),
		Entry(nil, "sacct", "-o", slurmopts.Required),
		Entry(nil, "sacct", "--units", slurmopts.Required),
		Entry(nil, "scontrol", "-o", slurmopts.None),
		Entry(nil, "sinfo", "-N", slurmopts.None),
	)

	It("says whether an option token takes the next token as its value", func() {
		sbatch := slurmopts.For("sbatch")
		Expect(sbatch.TakesNext("--qos")).To(BeTrue())
		Expect(sbatch.TakesNext("--qos=normal")).To(BeFalse())
		Expect(sbatch.TakesNext("-q")).To(BeTrue())
		Expect(sbatch.TakesNext("-qnormal")).To(BeFalse())
		Expect(sbatch.TakesNext("--wait")).To(BeFalse())
		Expect(sbatch.TakesNext("--exclusive")).To(BeFalse(), "an optional value is only ever attached")
		Expect(sbatch.TakesNext("--container-image")).To(BeFalse(), "not a SLURM option")
	})
})
