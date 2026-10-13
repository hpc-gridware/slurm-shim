package submit

// InputVar is one of SLURM's input environment variables: a default for the
// option Flag, set by a site module or a user's shell. Bool marks a flag that
// takes no value, set by the variable's presence.
type InputVar struct {
	Env, Flag string
	Bool      bool
}

// InputEnv turns the input variables that are set into option tokens. The
// caller places them after the script's directives and before the command
// line, which gives SLURM's precedence: command line, then environment, then
// directive. Only variables whose option the shim translates are listed; the
// rest are site defaults for concepts Grid Engine does not have, and warning
// about them on every submission would be noise the user cannot act on.
func InputEnv(vars []InputVar, lookup func(string) (string, bool)) []string {
	var tokens []string
	for _, v := range vars {
		val, ok := lookup(v.Env)
		if !ok {
			continue
		}
		switch {
		case v.Bool:
			tokens = append(tokens, "--"+v.Flag)
		case v.Flag == "exclusive" && val == "":
			tokens = append(tokens, "--exclusive")
		case val != "":
			tokens = append(tokens, "--"+v.Flag+"="+val)
		}
	}
	return tokens
}

// SbatchInputEnv are sbatch's input variables (SLURM 24.05) for the options
// the shim translates.
var SbatchInputEnv = []InputVar{
	{Env: "SBATCH_ACCOUNT", Flag: "account"},
	{Env: "SBATCH_ARRAY_INX", Flag: "array"},
	{Env: "SBATCH_CONSTRAINT", Flag: "constraint"},
	{Env: "SBATCH_ERROR", Flag: "error"},
	{Env: "SBATCH_EXCLUSIVE", Flag: "exclusive"},
	{Env: "SBATCH_EXPORT", Flag: "export"},
	{Env: "SBATCH_GPU_BIND", Flag: "gpu-bind"},
	{Env: "SBATCH_GPUS", Flag: "gpus"},
	{Env: "SBATCH_GPUS_PER_NODE", Flag: "gpus-per-node"},
	{Env: "SBATCH_GPUS_PER_TASK", Flag: "gpus-per-task"},
	{Env: "SBATCH_GRES", Flag: "gres"},
	{Env: "SBATCH_JOB_NAME", Flag: "job-name"},
	{Env: "SBATCH_MEM_PER_CPU", Flag: "mem-per-cpu"},
	{Env: "SBATCH_MEM_PER_GPU", Flag: "mem-per-gpu"},
	{Env: "SBATCH_MEM_PER_NODE", Flag: "mem"},
	{Env: "SBATCH_NO_REQUEUE", Flag: "no-requeue", Bool: true},
	{Env: "SBATCH_OUTPUT", Flag: "output"},
	{Env: "SBATCH_PARTITION", Flag: "partition"},
	{Env: "SBATCH_REQUEUE", Flag: "requeue", Bool: true},
	{Env: "SBATCH_SIGNAL", Flag: "signal"},
	{Env: "SBATCH_TIMELIMIT", Flag: "time"},
	{Env: "SBATCH_WAIT", Flag: "wait", Bool: true},
}

// SallocInputEnv are salloc's input variables for the options the shim
// translates.
var SallocInputEnv = []InputVar{
	{Env: "SALLOC_ACCOUNT", Flag: "account"},
	{Env: "SALLOC_CONSTRAINT", Flag: "constraint"},
	{Env: "SALLOC_EXCLUSIVE", Flag: "exclusive"},
	{Env: "SALLOC_GPUS", Flag: "gpus"},
	{Env: "SALLOC_GPUS_PER_NODE", Flag: "gpus-per-node"},
	{Env: "SALLOC_GPUS_PER_TASK", Flag: "gpus-per-task"},
	{Env: "SALLOC_GRES", Flag: "gres"},
	{Env: "SALLOC_IMMEDIATE", Flag: "immediate"},
	{Env: "SALLOC_MEM_PER_CPU", Flag: "mem-per-cpu"},
	{Env: "SALLOC_MEM_PER_GPU", Flag: "mem-per-gpu"},
	{Env: "SALLOC_MEM_PER_NODE", Flag: "mem"},
	{Env: "SALLOC_PARTITION", Flag: "partition"},
	{Env: "SALLOC_TIMELIMIT", Flag: "time"},
}

// SrunInputEnv are srun's SRUN_* input variables the shim honours. Since
// SLURM 22.05 a step no longer inherits --cpus-per-task from its job, and
// sites restore it with SRUN_CPUS_PER_TASK=$SLURM_CPUS_PER_TASK.
var SrunInputEnv = []InputVar{
	{Env: "SRUN_CPUS_PER_TASK", Flag: "cpus-per-task"},
	{Env: "SRUN_EXPORT_ENV", Flag: "export"},
}
