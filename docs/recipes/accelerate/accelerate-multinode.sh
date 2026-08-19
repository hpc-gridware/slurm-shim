#!/bin/bash
#SBATCH --job-name=accelerate-multinode
#SBATCH --partition=gpu
#SBATCH --nodes=2
#SBATCH --ntasks-per-node=1
#SBATCH --gpus-per-node=4
#SBATCH --output=accelerate-%j.out
# Multi-node Hugging Face Accelerate via the "srun bootstrap": srun runs ONE
# `accelerate launch` per node (--ntasks-per-node=1). Each launch reads the shim's
# SLURM_* env -- SLURM_PROCID as the machine rank, SLURM_NNODES as the machine
# count, and the head node from SLURM_JOB_NODELIST -- and accelerate (via torchrun)
# forms one process group of num_processes ranks across all nodes. This is HF's
# documented multi-node SLURM pattern; the shim just supplies the SLURM env.
set -uo pipefail
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh

# A Torch + Accelerate Python must be on PATH on every node. Like SLURM, sbatch
# forwards the submit-time environment (--export=ALL is the default), so a shared
# venv on PATH at submit time just works; use ENV_SETUP (module load, conda
# activate, ...) when the compute nodes need their own setup.
${ENV_SETUP:-true}

# The batch script is spooled by the scheduler (on SLURM too), so `$0`-relative
# paths do not resolve; the job runs in the submit directory and
# SLURM_SUBMIT_DIR points at it -- the standard SLURM pattern.
TRAIN="${TRAIN_SCRIPT:-$SLURM_SUBMIT_DIR/accelerate_train.py}"

GPUS_PER_NODE="${SLURM_GPUS_ON_NODE:-1}"
HEAD="$(scontrol show hostnames "$SLURM_JOB_NODELIST" | head -n1)"
PORT="${MASTER_PORT:-29500}"

# --machine_rank is per-node, so it is deferred (\$SLURM_PROCID) to each srun task's
# shell; everything else is identical on every node and is expanded here.
srun bash -c "python -m accelerate.commands.launch \
  --multi_gpu \
  --num_processes $((SLURM_NNODES * GPUS_PER_NODE)) \
  --num_machines $SLURM_NNODES \
  --machine_rank \$SLURM_PROCID \
  --main_process_ip $HEAD \
  --main_process_port $PORT \
  --rdzv_backend c10d \
  $TRAIN"
