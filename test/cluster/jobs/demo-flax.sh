#!/bin/bash
# Flax demo: multi-node data-parallel training submitted through the shim's
# `sbatch`. One task per node, two CPU devices each, so the six-device global mesh
# spans both processes and devices. The PE start_proc_args hook fabricates the
# SLURM_* environment; jax.distributed.initialize() auto-detects the job from it
# and the ranks form one training run whose gradients are reduced across the nodes.
#
# This is docs/recipes/flax/flax-cpu-check.sh with the container's venv path
# baked in -- same job script, same workload.
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=1


PYTHON="${PYTHON_BIN:-/home/gridware/flaxenv/bin/python}"

# Two CPU devices per process: jax.device_count() is then SLURM_NTASKS x 2, so the
# mesh spans devices as well as processes (as it would with 2 GPUs per node).
export JAX_NUM_CPU_DEVICES="${JAX_NUM_CPU_DEVICES:-2}"
# Fewer steps than the recipe default: every step is a real cross-container gloo
# all-reduce, which is far slower here than NCCL on a real GPU cluster, and the
# demo harness only waits a few minutes for the job.
export STEPS="${STEPS:-25}"
# gRPC honors proxy variables and will try to route the rendezvous through them.
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY

echo "[alloc] SLURM_NNODES=$SLURM_NNODES SLURM_NTASKS=$SLURM_NTASKS"
echo "[alloc] nodelist=$SLURM_JOB_NODELIST"
srun "$PYTHON" "$SLURM_SUBMIT_DIR/flax_dp_train.py"
