#!/bin/bash
#SBATCH --job-name=jax-cpu-check
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=1
#SBATCH --output=jax-cpu-%j.out
# CPU-only multi-process JAX: the same code path as the GPU recipe (auto-detect,
# coordinator rendezvous, cross-host collective) minus the devices. Multi-process
# CPU works out of the box since JAX 0.5.1, where gloo became the default CPU
# collectives implementation. Use this to verify a cluster before committing GPUs.
set -uo pipefail
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh

# A JAX environment must be on PATH on every node. sbatch forwards the submit
# environment, so a shared venv already on PATH when you submit needs nothing here;
# otherwise activate it (module load / source .../activate).

# Give each process more than one CPU device to exercise sharding across devices;
# jax.device_count() should then be SLURM_NTASKS * JAX_NUM_CPU_DEVICES.
export JAX_NUM_CPU_DEVICES="${JAX_NUM_CPU_DEVICES:-1}"
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY

srun python "${TRAIN_SCRIPT:-$SLURM_SUBMIT_DIR/jax_check.py}"
