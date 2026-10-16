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

package submit_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hpc-gridware/slurm-shim/internal/submit"
)

var _ = Describe("InputEnv [SBATCH_*, SALLOC_*, SRUN_*]", func() {
	lookupIn := func(env map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	}

	It("turns the set variables into option tokens, in the table's order", func() {
		env := map[string]string{
			"SBATCH_TIMELIMIT":   "10",
			"SBATCH_JOB_NAME":    "nightly",
			"SBATCH_REQUEUE":     "",
			"SBATCH_MEM_PER_CPU": "2G",
		}
		Expect(submit.InputEnv(submit.SbatchInputEnv, lookupIn(env))).To(Equal([]string{
			"--job-name=nightly", "--mem-per-cpu=2G", "--requeue", "--time=10",
		}))
	})

	It("reads an empty SBATCH_EXCLUSIVE as bare --exclusive and keeps a mode", func() {
		Expect(submit.InputEnv(submit.SbatchInputEnv, lookupIn(map[string]string{"SBATCH_EXCLUSIVE": ""}))).
			To(Equal([]string{"--exclusive"}))
		Expect(submit.InputEnv(submit.SallocInputEnv, lookupIn(map[string]string{"SALLOC_EXCLUSIVE": "user"}))).
			To(Equal([]string{"--exclusive=user"}))
	})

	It("skips an empty value and every variable it does not list", func() {
		env := map[string]string{"SBATCH_ACCOUNT": "", "SBATCH_QOS": "high", "SRUN_CPUS_PER_TASK": "4"}
		Expect(submit.InputEnv(submit.SbatchInputEnv, lookupIn(env))).To(BeEmpty())
		Expect(submit.InputEnv(submit.SrunInputEnv, lookupIn(env))).To(Equal([]string{"--cpus-per-task=4"}))
	})
})
