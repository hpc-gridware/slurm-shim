package sacct

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("request recovered from submit_cmd_line", func() {
	type want struct{ timelimit, reqMem, workDir, perHost string }

	DescribeTable("reads h_rt, the memory complex, -wd and -par from the qsub options",
		func(line string, w want) {
			req := parseSubmitLine(line, "mem_free")
			Expect(req.timelimit()).To(Equal(w.timelimit))
			Expect(req.reqMem()).To(Equal(w.reqMem))
			Expect(req.workDir).To(Equal(w.workDir))
			Expect(req.perHost).To(Equal(w.perHost))
		},
		// Lines below are verbatim submit_cmd_line values from OCS 9.1.6.
		Entry("h_rt in seconds",
			"qsub -clear -terse -o /dev/null -j y -q all.q -b y -N sx-hrt -l h_rt=5 sleep 60",
			want{timelimit: "00:00:05"}),
		Entry("memory, h_rt and -wd in one -l list",
			"qsub -clear -terse -o /dev/null -j y -q all.q -b y -N sx-wd -wd /tmp -l mem_free=100M,h_rt=600 sleep 2",
			want{timelimit: "00:10:00", reqMem: "100M", workDir: "/tmp"}),
		Entry("no h_rt on the command line is unknown: a #$ line or the queue may set one",
			"qsub -clear -terse -o /dev/null -j y -q all.q -N sx-emb /tmp/sx-embed.sh",
			want{}),
		Entry("-pe takes two values and -par one",
			"qsub -clear -terse -o /dev/null -j y -q all.q -N sx-pe -pe slurm-shim 4 -par 2 /tmp/sx-pe.sh",
			want{perHost: "2"}),
		Entry("h_rt in h:m:s, a G value raised exactly",
			"qsub -l h_rt=1:30:00 -l mem_free=4096M job.sh", want{timelimit: "01:30:00", reqMem: "4G"}),
		Entry("a day-long limit", "qsub -l h_rt=90000 job.sh", want{timelimit: "1-01:00:00"}),
		Entry("INFINITY", "qsub -l h_rt=INFINITY job.sh", want{timelimit: "UNLIMITED"}),
		Entry("a later -l replaces an earlier one", "qsub -l h_rt=60 -l h_rt=120 job.sh",
			want{timelimit: "00:02:00"}),
		Entry("a soft request is not a limit", "qsub -soft -l h_rt=60 -hard -l mem_free=1G job.sh",
			want{reqMem: "1G"}),
		Entry("a master-scoped request does not bound the job",
			"qsub -scope master -l h_rt=60 -scope global -l mem_free=1500M job.sh",
			want{reqMem: "1500M"}),
		Entry("the job's own arguments are not qsub's", "qsub -b y prog -x 60 /x",
			want{}),
		Entry("the job's own arguments that look like qsub's leave the request unknown",
			"qsub -l h_rt=300 -b y prog -l h_rt=60 -wd /x", want{}),
		Entry("a job name with a space hides the request after it",
			"qsub -N my run -l h_rt=300,mem_free=1G job.sh", want{}),
		Entry("a -wd with a space is not cut short",
			"qsub -l h_rt=300 -wd /data/my dir -l mem_free=1G job.sh", want{}),
		Entry("a spaced value after the request does not hide it",
			"qsub -l h_rt=300 -wd /data/my dir job.sh", want{timelimit: "00:05:00", workDir: "/data/my"}),
		Entry("-cwd names no directory", "qsub -wd /a -cwd job.sh", want{}),
		Entry("a non-site memory complex is not ReqMem", "qsub -l h_vmem=2G job.sh",
			want{}),
		Entry("an unreadable h_rt prints nothing", "qsub -l h_rt=soon job.sh", want{}),
		Entry("no recorded command line prints nothing", "", want{}),
	)

	DescribeTable("renders ReqMem in SLURM's exact-unit form",
		func(ge, slurm string) {
			Expect(jobRequest{mem: ge}.reqMem()).To(Equal(slurm))
		},
		Entry("bytes, rounded up to a megabyte", "1048577", "2M"),
		Entry("K", "2048K", "2M"),
		Entry("G", "4G", "4G"),
		Entry("T", "1T", "1T"),
		Entry("decimal g", "1g", "954M"),
		Entry("non-exact stays in M", "1500M", "1500M"),
		Entry("garbage", "lots", ""),
		Entry("none", "", ""),
	)
})
