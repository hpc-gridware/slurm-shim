#!/bin/bash
#SBATCH --job-name=deepspeed-train
#SBATCH --partition=gpu
#SBATCH --nodes=2
#SBATCH --ntasks-per-node=1
#SBATCH --cpus-per-task=32
#SBATCH --output=deepspeed-%j.out
#
# DeepSpeed multi-node training on the slurm-shim (GE tight integration).
#
# Pattern: one srun task per NODE (--ntasks-per-node=1); each task launches a
# torchrun that forks one worker per GPU. torchrun sets RANK/LOCAL_RANK/WORLD_SIZE
# for torch.distributed, which DeepSpeed consumes directly -- so there is no MPI
# bootstrap and no silent-singleton footgun. Submit with: sbatch deepspeed-multinode.sh
set -euo pipefail

# Rendezvous host: master is the first name in the nodelist (MASTER_ADDR is off
# by default on the shim, so derive it).
MASTER_ADDR="$(scontrol show hostnames | head -n1)"
MASTER_PORT="${MASTER_PORT:-29500}"

# GPUs per node comes straight from the fabricated environment.
GPUS_PER_NODE="${SLURM_GPUS_ON_NODE:-8}"

echo "nodes=${SLURM_NNODES} gpus/node=${GPUS_PER_NODE} master=${MASTER_ADDR}:${MASTER_PORT}"

# One torchrun per node. --export=ALL forwards the fabricated SLURM_* env; each
# node's single task sees that node's whole GPU set, so torchrun's workers index
# devices by LOCAL_RANK as usual.
srun --ntasks-per-node=1 --export=ALL bash -c '
  exec torchrun \
    --nnodes="${SLURM_NNODES}" \
    --nproc_per_node='"${GPUS_PER_NODE}"' \
    --node_rank="${SLURM_NODEID}" \
    --master_addr='"${MASTER_ADDR}"' \
    --master_port='"${MASTER_PORT}"' \
    train.py --deepspeed --deepspeed_config ds_config.json "$@"
' _ "$@"
