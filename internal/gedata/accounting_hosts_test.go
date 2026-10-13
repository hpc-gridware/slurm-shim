package gedata_test

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

var _ = Describe("JobAccounting request and host facts", func() {
	get := func(out string) []gedata.AccountingRecord {
		recs, err := gedata.JobAccounting(context.Background(), acctRunner(out),
			gedata.AcctQuery{JobID: "1391"})
		Expect(err).NotTo(HaveOccurred())
		return recs
	}

	It("keeps the PE task hosts of a live OCS 9.1.6 PE job on its master record", func() {
		// Two qrsh -inherit tasks (1.ocs-worker1, 1.ocs-master), then the master
		// record on ocs-worker1, as qacct printed them.
		b, err := os.ReadFile("testdata/qacct-pe-tasks-9.1.6.txt")
		Expect(err).NotTo(HaveOccurred())
		recs := get(string(b))
		Expect(recs).To(HaveLen(1))
		Expect(recs[0].Host).To(Equal("ocs-worker1"))
		Expect(recs[0].Hosts).To(Equal([]string{"ocs-worker1", "ocs-master"}))
		Expect(recs[0].PETasks).To(Equal(2))
		Expect(recs[0].GrantedPE).To(Equal("slurm-shim"))
		Expect(recs[0].SubmitCmdLine).To(ContainSubstring("-pe slurm-shim 4 -par 2"))
	})

	It("attributes task records to the run they precede when a job ran twice", func() {
		rec := func(pe, host string) string {
			return qacctSep + "\njobnumber 1391\ntaskid undefined\npe_taskid " + pe +
				"\nhostname " + host + "\ngranted_pe slurm-shim\nslots 4\nfailed 0\nexit_status 0\n"
		}
		out := rec("1.n2", "n2") + rec("NONE", "n1") + rec("NONE", "n3")
		recs := get(out)
		Expect(recs).To(HaveLen(2))
		Expect(recs[0].Hosts).To(Equal([]string{"n1", "n2"}))
		Expect(recs[0].PETasks).To(Equal(1))
		Expect(recs[1].Hosts).To(Equal([]string{"n3"}))
		Expect(recs[1].PETasks).To(Equal(0))
	})

	It("reports no PE for qacct's NONE", func() {
		recs := get(qacctSep + "\njobnumber 1391\ntaskid undefined\ngranted_pe NONE\nhostname n1\nfailed 0\nexit_status 0\n")
		Expect(recs[0].GrantedPE).To(BeEmpty())
		Expect(recs[0].Hosts).To(Equal([]string{"n1"}))
	})
})
