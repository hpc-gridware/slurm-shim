#!/bin/bash
#SBATCH --job-name=jax-multinode
#SBATCH --partition=gpu
#SBATCH --nodes=2
#SBATCH --ntasks-per-node=4      # MUST equal GPUs per node: JAX runs one process per GPU
#SBATCH --gpus-per-node=4
#SBATCH --cpus-per-task=8
#SBATCH --output=jax-%j.out
#
# Node width: this asks for 32 slots on each of 2 nodes (4 task(s) x
# 8 cpus-per-task), which models a real 4-GPU / 32-core node. On OCS 9.1.5+ the
# shim pins that layout
# with `qsub -par 32`, so a cluster whose nodes are narrower refuses the job at
# submit. That is not new strictness -- such a request could never have run; it
# used to wait in the queue forever for a node that does not exist.
#
# To run it on a small cluster, override the geometry on the command line (CLI
# flags win over #SBATCH). Verified on the shipped 3 x 14-slot test cluster:
#
#   sbatch -N 2 --ntasks-per-node=1 --cpus-per-task=2 --gpus-per-node=1 jax-multinode.sh
#
# Multi-node JAX. This is the standard SLURM shape -- unchanged from what you would
# run on a real SLURM cluster -- because JAX needs no launcher glue: it reads five
# SLURM_* variables, derives its own coordinator address, and rendezvouses over its
# own gRPC service. No PMI, no MASTER_ADDR, no torchrun.
#
#   SLURM_PROCID  -> process_id        SLURM_NTASKS   -> process_count
#   SLURM_LOCALID -> local device      SLURM_STEP_NODELIST -> coordinator host
#
# Do NOT use --gpus-per-task here: that binds each task to a single device, and JAX
# indexes into the visible list by SLURM_LOCALID, so ranks above 0 would fail with
# CUDA_ERROR_INVALID_DEVICE. Ask for GPUs per NODE and let JAX do the splitting.
set -uo pipefail
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh

# A JAX environment must be on PATH on every node. sbatch forwards the submit
# environment (--export=ALL, like SLURM), so a shared venv already on PATH when you
# submit needs nothing here. Otherwise activate it below, e.g.:
#   module load jax
#   source /shared/jaxenv/bin/activate

# Optional but recommended on shared nodes: JAX preallocates ~75% of each visible
# device by default.
export XLA_PYTHON_CLIENT_MEM_FRACTION="${XLA_PYTHON_CLIENT_MEM_FRACTION:-0.90}"

# JAX derives its coordinator port from SLURM_JOB_ID % 4096 + 61440, so two jobs
# whose ids differ by a multiple of 4096 collide. Array tasks share one job id and
# would ALL collide, so give each task its own port.
if [ -n "${SLURM_ARRAY_TASK_ID:-}" ]; then
  export JAX_COORDINATOR_PORT=$(( 61440 + (SLURM_ARRAY_JOB_ID + SLURM_ARRAY_TASK_ID) % 4096 ))
fi

# gRPC honors proxy variables and will try to route the rendezvous through them.
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY

srun python "${TRAIN_SCRIPT:-$SLURM_SUBMIT_DIR/jax_check.py}"
