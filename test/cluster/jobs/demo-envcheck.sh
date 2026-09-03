#!/bin/bash
# Environment diagnosis: shows where the fabricated SLURM_* environment comes
# from, and -- when it is missing -- which of the two wiring steps failed.
#
# `scontrol show hostnames` with no argument is a pure function of
# $SLURM_JOB_NODELIST. Empty output therefore never means "scontrol is broken";
# it means the PE start_proc_args hook did not run, or this script did not
# source its output. This job prints both sides of that boundary.
#
#   test/cluster/demo.sh envcheck     (or: make demo-envcheck)
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=2

state="${TMPDIR:-/tmp}/slurm_shim"

echo "[where] host=$(hostname) TMPDIR=${TMPDIR:-/tmp}"
echo

# (1) Before the hook. The job inherits the submit environment (sbatch defaults
# to --export=ALL -> qsub -V), so anything SLURM_* here came from the submitting
# shell, not from the fabricator.
echo "[1/4] before sourcing the hook: $(env | grep -c '^SLURM_') SLURM_* variables"
echo "      scontrol show hostnames -> '$(scontrol show hostnames)'"
echo

# (2) What the PE start_proc_args hook left behind. The fabricator runs on the
# master host only and exits 0 even on failure (a non-zero start_proc_args would
# error the queue instance), so the sentinel is the real failure signal.
echo "[2/4] fabricator state dir $state"
if [ -d "$state" ]; then
  ls -l "$state"
  [ -e "$state/environment.failed" ] && echo "      !! environment.failed present: fabrication FAILED"
else
  echo "      MISSING -- the PE has no start_proc_args hook."
  echo "      check: qconf -sp make | grep start_proc_args"
fi
echo

# (3) Source it, bare -- the hook enforces its own abort paths, so a failed
# fabrication ends the job here rather than letting it run degraded. The policy
# below additionally turns a *missing* environment (the fabricator never ran)
# into a failure; the default there is "continue".
echo "[3/4] sourcing the hook (SLURM_SHIM_HOOK_MISSING_ENV=abort)"
export SLURM_SHIM_HOOK_MISSING_ENV=abort
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh
echo "      ok, $(env | grep -c '^SLURM_') SLURM_* variables now set"
echo

# (4) The same call that printed nothing above. The nodelist round-trips through
# the compressed form, and CompressNodelist preserves input order, so the first
# line is the master -- which is why the recipes derive MASTER_ADDR from it.
echo "[4/4] after the hook"
echo "      SLURM_JOB_NODELIST=$SLURM_JOB_NODELIST"
echo "      SLURM_NNODES=$SLURM_NNODES SLURM_NTASKS=$SLURM_NTASKS"
echo "      scontrol show hostnames:"
scontrol show hostnames | sed 's/^/        /'
echo "      head -n1 (= MASTER_ADDR): $(scontrol show hostnames | head -n1)"
