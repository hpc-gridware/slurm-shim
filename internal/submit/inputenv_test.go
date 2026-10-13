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
