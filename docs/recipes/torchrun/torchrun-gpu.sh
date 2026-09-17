#!/bin/bash
#SBATCH --job-name=torchrun-gpu
#SBATCH --partition=gpu
#SBATCH --nodes=4
#SBATCH --ntasks-per-node=1
#SBATCH --gpus-per-node=2
#SBATCH --output=torchrun-%j.out
# Multi-node torchrun over NCCL: one launcher per node, one worker per GPU.
# Each worker selects its GPU by LOCAL_RANK within CUDA_VISIBLE_DEVICES, which
# the shim sets from the Grid Engine RSMAP grant. Keep --gpus-per-node equal to
# --nproc_per_node below.
#
# Comments stay off the #SBATCH lines: slurm-shim up to v0.6.0 reads an inline
# comment there as arguments and drops the directives that follow.
#
# `srun --ntasks-per-node=1` is stated on the launcher line on purpose: on a PE
# with `task_policy: gpu` a plain srun would start one torchrun per GPU.
#
# NCCL_SOCKET_IFNAME is deliberately unset; NCCL chose the right NIC on L4.
# To see which interface it picks:
# export NCCL_DEBUG=INFO NCCL_DEBUG_SUBSYS=INIT,NET
#
# The rendezvous must be private to this job: two jobs whose first node is the
# same host would otherwise meet on one port. The id and port therefore include
# the array task (all tasks of an array job share SLURM_JOB_ID). The port is the
# shim's own MASTER_PORT when the site exports it, else the same formula over
# the shim's default range 20000-29999.
#
# torchrun must be on PATH on every node (shared venv, module, or container).
: "${SLURM_JOB_NODELIST:?no SLURM environment: is the queue starter_method set up?}"
head=$(scontrol show hostnames "$SLURM_JOB_NODELIST" | head -n1)
task=${SLURM_ARRAY_TASK_ID:-0}
port=${MASTER_PORT:-$((20000 + (SLURM_JOB_ID * 31 + task) % 10000))}
echo "HEAD=$head NNODES=$SLURM_NNODES"
srun --ntasks-per-node=1 torchrun --nnodes "$SLURM_NNODES" --nproc_per_node 2 \
  --rdzv_id "$SLURM_JOB_ID-$task" --rdzv_backend c10d \
  --rdzv_endpoint "$head:$port" \
  "${TRAIN_SCRIPT:-$SLURM_SUBMIT_DIR/allreduce_check.py}"
