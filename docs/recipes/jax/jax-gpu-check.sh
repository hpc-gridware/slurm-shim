#!/bin/bash
#SBATCH --job-name=jax-gpu-check
#SBATCH --partition=gpu
#SBATCH --nodes=4
#SBATCH --ntasks-per-node=1
#SBATCH --gpus-per-node=1
#SBATCH --output=jax-gpu-%j.out
# JAX multi-process on GPUs. jax.distributed.initialize() with no arguments
# auto-detects the job from the shim's SLURM_* environment. Every process must
# see all SLURM_NTASKS processes and every process's devices; it prints ok=1,
# and exits non-zero otherwise. One process per GPU: keep --ntasks-per-node
# equal to --gpus-per-node.
# (Comments stay off the #SBATCH lines: slurm-shim up to v0.6.0 reads an inline
# comment there as arguments and drops the directives that follow.)
#
# Launch through srun: JAX detects SLURM via SLURM_STEP_NODELIST, which exists
# only inside a step. Without it JAX quietly runs as a single process.
#
# PYTHON_BIN must be a JAX-with-CUDA environment present on every node.
srun "${PYTHON_BIN:-python3}" -c '
import os
import sys

import jax

jax.distributed.initialize()
nproc, local, total = jax.process_count(), jax.local_device_count(), jax.device_count()
ntasks = int(os.environ.get("SLURM_NTASKS", "0"))
ok = nproc == ntasks and total == nproc * local
print(f"JAX pid={jax.process_index()} nproc={nproc} local={local} global={total} "
      f"ntasks={ntasks} ok={int(ok)}", flush=True)
sys.exit(0 if ok else 1)
'
