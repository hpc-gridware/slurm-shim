#!/bin/bash
# Environment diagnosis: shows whether the fabricated SLURM_* environment reached
# this job and -- when it did not -- which wiring step is missing.
#
# With the queue's starter_method wired (docs/install/slurm-shim-starter.sh) the
# environment is present from the first line, and this script contains nothing
# shim-specific. On a site without a starter, a job script must source the hook
# itself:   . /opt/slurm-shim/etc/slurm-shim-source-hook.sh
#
# `scontrol show hostnames` with no argument is a pure function of
# $SLURM_JOB_NODELIST: empty output never means scontrol is broken, it means the
# environment never arrived.
#
#   test/cluster/demo.sh envcheck     (or: make demo-envcheck)
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=2

state="${TMPDIR:-/tmp}/slurm_shim"

echo "[where] host=$(hostname) TMPDIR=${TMPDIR:-/tmp}"
echo

n=$(env | grep -c '^SLURM_')
echo "[1/3] environment at script start: $n SLURM_* variables"
if [ "$n" -eq 0 ]; then
  echo "      NONE -- the queue's starter_method is not wired, or the hook file is missing."
  echo "      check:    qconf -sq <queue> | grep starter_method"
  echo "      fallback: add '. /opt/slurm-shim/etc/slurm-shim-source-hook.sh' to the script"
fi
echo

# What the PE start_proc_args hook left behind. The fabricator runs on the
# master host only and exits 0 even on failure, leaving a sentinel instead; a
# sentinel would have made the starter abort this job before it started.
echo "[2/3] fabricator state dir $state"
if [ -d "$state" ]; then
  ls -l "$state"
else
  echo "      MISSING -- the PE has no start_proc_args hook."
  echo "      check: qconf -sp <pe> | grep start_proc_args"
fi
echo

# The nodelist round-trips through the compressed form in allocation order, so
# the first line is the master -- which is why the recipes derive MASTER_ADDR
# from it.
echo "[3/3] scontrol show hostnames  (reads SLURM_JOB_NODELIST=${SLURM_JOB_NODELIST:-<unset>})"
scontrol show hostnames | sed 's/^/        /'
echo "      head -n1 (= MASTER_ADDR): $(scontrol show hostnames | head -n1)"
