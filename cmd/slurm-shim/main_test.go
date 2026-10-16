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

package main

import (
	"bytes"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/slurmopts"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("arg0 dispatch", func() {
	var stdout, stderr *bytes.Buffer

	BeforeEach(func() {
		stdout = &bytes.Buffer{}
		stderr = &bytes.Buffer{}
	})

	It("treats symlink and subcommand forms identically [REQ-DLV-001]", func() {
		viaSymlink := run("srun", []string{"--version"}, stdout, stderr)

		out2 := &bytes.Buffer{}
		viaSubcommand := run("slurm-shim", []string{"srun", "--version"}, out2, &bytes.Buffer{})

		Expect(viaSymlink).To(Equal(viaSubcommand))
		Expect(stdout.String()).To(Equal(out2.String()))
	})

	DescribeTable("every shimmed command answers --version and -V [REQ-DLV-003]",
		func(arg0 string, flag string) {
			out := &bytes.Buffer{}
			code := run(arg0, []string{flag}, out, &bytes.Buffer{})
			Expect(code).To(Equal(0))
			Expect(out.String()).To(HavePrefix("slurm "))
			Expect(out.String()).To(ContainSubstring("slurm-shim "))
		},
		Entry("srun --version", "srun", "--version"),
		Entry("sbatch -V", "sbatch", "-V"),
		Entry("sinfo -V", "sinfo", "-V"),
		Entry("salloc --version", "salloc", "--version"),
		Entry("scontrol --version", "scontrol", "--version"),
		Entry("squeue --version", "squeue", "--version"),
		Entry("scancel -V", "scancel", "-V"),
	)

	It("leaves a --version after the script or command to the script or command", func() {
		Expect(hasVersionFlag([]string{"job.sh", "--version"}, slurmopts.For("sbatch"))).To(BeFalse())
		Expect(hasVersionFlag([]string{"-n", "2", "python", "-V"}, slurmopts.For("srun"))).To(BeFalse())
		Expect(hasVersionFlag([]string{"-p", "-V"}, slurmopts.For("sbatch"))).To(BeFalse(), "-V is -p's value")
		Expect(hasVersionFlag([]string{"-p", "gpu", "-V"}, slurmopts.For("sbatch"))).To(BeTrue())
		Expect(hasVersionFlag([]string{"sbatch", "job.sh", "-V"}, slurmopts.Table{})).To(BeFalse())
	})

	It("prints version to stdout, never stderr [REQ-LOG-003]", func() {
		run("srun", []string{"--version"}, stdout, stderr)
		Expect(stdout.Len()).To(BeNumerically(">", 0))
		Expect(stderr.Len()).To(Equal(0))
	})

	It("errors with the command-name prefix for unimplemented commands [REQ-LOG-001]", func() {
		code := run("srun", []string{"hostname"}, stdout, stderr)
		Expect(code).To(Equal(1))
		Expect(stdout.Len()).To(Equal(0))
		Expect(stderr.String()).To(HavePrefix("srun: error:"))
	})

	It("rejects an unknown command name [REQ-DLV-001]", func() {
		code := run("notacommand", nil, stdout, stderr)
		Expect(code).To(Equal(1))
		Expect(stderr.String()).To(HavePrefix("slurm-shim: error: unknown command"))
	})

	It("rejects the bare binary with no subcommand", func() {
		code := run("slurm-shim", nil, stdout, stderr)
		Expect(code).To(Equal(1))
		Expect(stderr.String()).To(ContainSubstring("no command given"))
	})

	It("keeps all diagnostics off stdout [REQ-LOG-003]", func() {
		run("notacommand", nil, stdout, stderr)
		Expect(stdout.Len()).To(Equal(0))
		Expect(strings.TrimSpace(stderr.String())).NotTo(BeEmpty())
	})
})

// These drive the real Run() entry points, so they fail if dryrun.Wrap is ever
// removed from a mutating command's wiring -- the unit specs drive the unexported
// run() and would stay green.
var _ = Describe("dry-run wiring at the command entry points [SLURM_SHIM_DRY_RUN]", func() {
	var stdout, stderr *bytes.Buffer

	BeforeEach(func() {
		GinkgoT().Setenv("SLURM_SHIM_DRY_RUN", "1")
		stdout = &bytes.Buffer{}
		stderr = &bytes.Buffer{}
	})

	It("intercepts scancel before it can reach qdel", func() {
		code := run("scancel", []string{"4711"}, stdout, stderr)

		Expect(code).To(Equal(0))
		Expect(stderr.String()).To(ContainSubstring("dry run"))
		Expect(stderr.String()).To(ContainSubstring("would run: qdel 4711"))
		Expect(stdout.String()).To(BeEmpty(), "real scancel writes nothing to stdout")
	})

	It("intercepts scontrol requeue before it can reach qmod", func() {
		code := run("scontrol", []string{"requeue", "4711_2"}, stdout, stderr)

		Expect(code).To(Equal(0))
		Expect(stderr.String()).To(ContainSubstring("would run: qmod -rj 4711.3"))
		Expect(stdout.String()).To(BeEmpty())
	})
})
