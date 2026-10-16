/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

package sbatch

import (
	"bytes"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// oldUsage is a qsub -help without -par (OCS before 9.1.5).
const oldUsage = "OCS 9.1.4 (250601-0000)\nusage: qsub [options]\n"

var _ = Describe("sbatch hot spares (--x-spares)", func() {
	var cfg *config.Config
	BeforeEach(func() {
		cfg = testCfg()
		// The submit environment is an input (todo 138); start every spec without it.
		for _, k := range []string{"SLURM_SHIM_SPARES", "SLURM_X_ELASTIC"} {
			if v, ok := os.LookupEnv(k); ok {
				Expect(os.Unsetenv(k)).To(Succeed())
				DeferCleanup(os.Setenv, k, v)
			}
		}
	})
	withDefault := func(partition string, spares int) {
		p := cfg.Partitions[partition]
		p.Spares = spares
		cfg.Partitions[partition] = p
	}
	submitScript := func(usage, body string, args ...string) ([]string, string, int) {
		var captured []string
		script := filepath.Join(GinkgoT().TempDir(), "job.sh")
		Expect(os.WriteFile(script, []byte(body), 0o700)).To(Succeed())
		var out, errOut bytes.Buffer
		code := run(parRunner(usage, &captured), cfg, "/usr/bin/slurm-shim", append(args, script), &out, &errOut)
		return captured, errOut.String(), code
	}

	It("widens the request by k nodes of the same width and tells the job", func() {
		argv, _, code := submitWith(cfg, parUsage, "-p", "gpu", "-N", "4", "--ntasks-per-node=2", "--x-spares=1")
		Expect(code).To(Equal(0))
		// 4 nodes x 2 slots + 1 spare x 2 slots = 10 slots, still 2 per node.
		Expect(argv).To(ContainElements("-pe", "gpu.pe", "10", "-par", "2"))
		Expect(argv).To(ContainElements("-v", "SLURM_SHIM_SPARES=1"))
	})

	It("carries --x-elastic into the job", func() {
		argv, _, _ := submitWith(cfg, parUsage, "-p", "gpu", "-N", "2", "--x-spares=1", "--x-elastic=on")
		Expect(argv).To(ContainElements("-v", "SLURM_X_ELASTIC=on"))
	})

	It("reads --x-spares from a #SHIM line, which real SLURM ignores", func() {
		argv, errOut, code := submitScript(parUsage, "#!/bin/bash\n#SBATCH -p gpu -N 2\n#SHIM --x-spares=1\nsrun hostname\n")
		Expect(code).To(Equal(0), errOut)
		Expect(argv).To(ContainElements("-pe", "gpu.pe", "3", "-v", "SLURM_SHIM_SPARES=1"))
	})

	It("does not read '#SHIMX' as a #SHIM line", func() {
		argv, errOut, code := submitScript(parUsage, "#!/bin/bash\n#SBATCH -p gpu -N 2\n#SHIMX --x-spares=1\nsrun hostname\n")
		Expect(code).To(Equal(0), errOut)
		Expect(argv).To(ContainElements("-pe", "gpu.pe", "2"))
		Expect(argv).NotTo(ContainElement("SLURM_SHIM_SPARES=1"))
	})

	It("ignores a #SHIM line after the first command", func() {
		argv, errOut, code := submitScript(parUsage, "#!/bin/bash\n#SBATCH -p gpu -N 2\nsrun hostname\n#SHIM --x-spares=1\n")
		Expect(code).To(Equal(0), errOut)
		Expect(argv).To(ContainElements("-pe", "gpu.pe", "2"))
		Expect(argv).NotTo(ContainElement("SLURM_SHIM_SPARES=1"))
	})

	It("lets the command line override a #SHIM line", func() {
		argv, errOut, code := submitScript(parUsage, "#!/bin/bash\n#SBATCH -p gpu -N 2\n#SHIM --x-spares=1\nsrun hostname\n",
			"--x-spares=0")
		Expect(code).To(Equal(0), errOut)
		Expect(argv).To(ContainElements("-pe", "gpu.pe", "2"))
		Expect(argv).NotTo(ContainElement("SLURM_SHIM_SPARES=1"))
	})

	It("takes the partition's default when no flag is given", func() {
		withDefault("gpu", 1)
		argv, _, _ := submitWith(cfg, parUsage, "-p", "gpu", "-N", "2")
		Expect(argv).To(ContainElements("-pe", "gpu.pe", "3", "-v", "SLURM_SHIM_SPARES=1"))
	})

	// todo 141: the user never asked for the default, so a job that cannot take
	// spares is submitted without them, with a warning, instead of refused.
	DescribeTable("submits a job that cannot take the partition's default without spares",
		func(partition, usage string, args []string, wantSlots, why string) {
			withDefault(partition, 1)
			argv, errOut, code := submitWith(cfg, usage, append([]string{"-p", partition}, args...)...)
			Expect(code).To(Equal(0), errOut)
			Expect(errOut).To(ContainSubstring("sbatch: warning: partition " + partition +
				" defaults to 1 spare(s), but this job cannot take them"))
			Expect(errOut).To(ContainSubstring(why))
			Expect(errOut).To(ContainSubstring("submitting without spares"))
			Expect(argv).To(ContainElements("-pe", cfg.Partitions[partition].PE, wantSlots))
			Expect(argv).NotTo(ContainElement("SLURM_SHIM_SPARES=1"))
		},
		Entry("no --nodes", "gpu", parUsage, []string{"-n", "4"}, "4", "not pinned"),
		Entry("a 1-node job", "gpu", parUsage, []string{"-N", "1"}, "1", "runs on 1 node"),
		Entry("OCS without qsub -par", "gpu", oldUsage, []string{"-N", "2"}, "2", "not pinned"),
		Entry("a literal slots rule", "batch", parUsage, []string{"-N", "2"}, "16", "not pinned"),
		Entry("a -w host list", "gpu", parUsage, []string{"-w", "n1,n2"}, "2", "exactly its --nodelist hosts"),
	)

	It("still refuses an explicit --x-spares the job cannot take on a defaulted partition", func() {
		withDefault("gpu", 1)
		_, errOut, code := submitWith(cfg, parUsage, "-p", "gpu", "-N", "1", "--x-spares=1")
		Expect(code).To(Equal(1))
		Expect(errOut).To(ContainSubstring("at least 2 nodes"))
	})

	It("refuses an explicit --x-spares with a -w host list: no other host may hold the spare", func() {
		_, errOut, code := submitWith(cfg, parUsage, "-p", "gpu", "-w", "n1,n2", "--x-spares=1")
		Expect(code).To(Equal(1))
		Expect(errOut).To(ContainSubstring("cannot be combined with --nodelist"))
	})

	It("lets --x-spares=0 switch a partition default off", func() {
		withDefault("gpu", 1)
		argv, _, _ := submitWith(cfg, parUsage, "-p", "gpu", "-N", "2", "--x-spares=0")
		Expect(argv).To(ContainElements("-pe", "gpu.pe", "2"))
		Expect(argv).NotTo(ContainElement("SLURM_SHIM_SPARES=1"))
	})

	DescribeTable("refuses spares that cannot work",
		func(usage string, args []string, why string) {
			_, errOut, code := submitWith(cfg, usage, args...)
			Expect(code).To(Equal(1))
			Expect(errOut).To(ContainSubstring(why))
		},
		Entry("a 1-node job: the master cannot be spared", parUsage,
			[]string{"-p", "gpu", "-N", "1", "--x-spares=1"}, "at least 2 nodes"),
		Entry("no layout stated", parUsage,
			[]string{"-p", "gpu", "-n", "4", "--x-spares=1"}, "at least 2 nodes"),
		Entry("OCS without qsub -par", oldUsage,
			[]string{"-p", "gpu", "-N", "2", "--x-spares=1"}, "OCS 9.1.5+"),
		Entry("a negative count", parUsage,
			[]string{"-p", "gpu", "-N", "2", "--x-spares=-1"}, "must not be negative"),
		Entry("an unknown elastic mode", parUsage,
			[]string{"-p", "gpu", "-N", "2", "--x-spares=1", "--x-elastic=maybe"}, "expected on, off or auto"),
	)

	It("enforces the site's max_spares", func() {
		cfg.MaxSpares = 1
		_, errOut, code := submitWith(cfg, parUsage, "-p", "gpu", "-N", "2", "--x-spares=2")
		Expect(code).To(Equal(1))
		Expect(errOut).To(ContainSubstring("exceeds this site's max_spares 1"))
	})

	// todo 138: -V forwards the submit environment, so a job submitted from inside a
	// spares job inherits SLURM_SHIM_SPARES; an explicit -v must override it.
	It("keeps every node of a job submitted from inside a spares job", func() {
		GinkgoT().Setenv("SLURM_SHIM_SPARES", "1")
		GinkgoT().Setenv("SLURM_X_ELASTIC", "on")
		argv, _, code := submitWith(cfg, parUsage, "-p", "gpu", "-N", "4")
		Expect(code).To(Equal(0))
		Expect(argv).To(ContainElements("-pe", "gpu.pe", "4", "-V", "-v", "SLURM_SHIM_SPARES=0"))
		Expect(argv).NotTo(ContainElement(HavePrefix("SLURM_X_ELASTIC")))
	})

	It("clears an inherited SLURM_X_ELASTIC when --x-elastic is not given", func() {
		GinkgoT().Setenv("SLURM_SHIM_SPARES", "3")
		GinkgoT().Setenv("SLURM_X_ELASTIC", "on")
		argv, _, code := submitWith(cfg, parUsage, "-p", "gpu", "-N", "2", "--x-spares=1")
		Expect(code).To(Equal(0))
		Expect(argv).To(ContainElements("-v", "SLURM_SHIM_SPARES=1", "-v", "SLURM_X_ELASTIC="))
	})

	It("leaves a job without spares exactly as before", func() {
		argv, _, _ := submitWith(cfg, parUsage, "-p", "gpu", "-N", "2")
		Expect(argv).To(ContainElements("-pe", "gpu.pe", "2"))
		for _, a := range argv {
			Expect(a).NotTo(HavePrefix("SLURM_SHIM_SPARES"))
			Expect(a).NotTo(HavePrefix("SLURM_X_ELASTIC"))
		}
	})
})
