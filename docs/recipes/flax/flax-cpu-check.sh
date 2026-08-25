#!/bin/bash
#SBATCH --job-name=flax-cpu-check
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=1
#SBATCH --output=flax-cpu-%j.out
# CPU-only data-parallel Flax: the same code path as the GPU recipe (auto-detect,
# coordinator rendezvous, global mesh, cross-host gradient all-reduce) minus the
# devices. Multi-process CPU works out of the box since JAX 0.5.1, where gloo
# became the default CPU collectives implementation. Use this to verify a cluster
# before committing GPUs -- it is the same script the container demo runs.
set -uo pipefail
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh

# jax, flax and optax must be importable on every node. sbatch forwards the submit
# environment, so a shared venv already on PATH when you submit needs nothing here;
# otherwise activate it (module load / source .../activate).

# Give each process more than one CPU device so the mesh spans devices as well as
# processes; jax.device_count() is then SLURM_NTASKS * JAX_NUM_CPU_DEVICES.
export JAX_NUM_CPU_DEVICES="${JAX_NUM_CPU_DEVICES:-2}"
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY

srun python "${TRAIN_SCRIPT:-$SLURM_SUBMIT_DIR/flax_dp_train.py}"
