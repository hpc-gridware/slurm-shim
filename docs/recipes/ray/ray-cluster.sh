#!/bin/bash
#SBATCH --job-name=ray-cluster
#SBATCH --partition=gpu
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=1
#SBATCH --output=ray-%j.out
#
# Bring up a Ray cluster inside a GE PE job on the slurm-shim, then run a driver
# against it. Ray runs its own head/worker gossip (not PMI), so srun only has to
# start one Ray process per node -- the "srun bootstrap" pattern. This is also
# the path multi-node vLLM (tensor/pipeline parallel) rides on.
# Submit with: sbatch ray-cluster.sh
set -euo pipefail

# Nodelist, master first (scontrol reads $SLURM_JOB_NODELIST).
mapfile -t NODES < <(scontrol show hostnames)
HEAD="${NODES[0]}"
HEAD_IP="$(getent hosts "$HEAD" | awk '{print $1}')"
PORT="${RAY_PORT:-6379}"
DASH_PORT="${RAY_DASHBOARD_PORT:-8265}"
GPUS_PER_NODE="${SLURM_GPUS_ON_NODE:-0}"
echo "Ray head ${HEAD} (${HEAD_IP}:${PORT}); ${#NODES[@]} nodes, ${GPUS_PER_NODE} gpu/node"

# Head (node 0). --block keeps the Ray process (and this srun step) alive.
srun --nodes=1 --ntasks=1 -w "$HEAD" \
  ray start --head --node-ip-address="$HEAD_IP" --port="$PORT" \
    --dashboard-host=0.0.0.0 --dashboard-port="$DASH_PORT" \
    --num-gpus="$GPUS_PER_NODE" --block &
sleep 15   # let the head bind before workers connect

# Workers (remaining nodes).
for worker in "${NODES[@]:1}"; do
  srun --nodes=1 --ntasks=1 -w "$worker" \
    ray start --address="${HEAD_IP}:${PORT}" \
      --num-gpus="$GPUS_PER_NODE" --block &
done
sleep 15   # let workers register

# Driver runs on the master (this script's host == the Ray head node).
export RAY_ADDRESS="${HEAD_IP}:${PORT}"
python train.py        # inside: ray.init(address="auto")

# Multi-node vLLM instead of a training driver would be, e.g.:
#   ray job submit --address="http://${HEAD_IP}:${DASH_PORT}" -- \
#     vllm serve <model> --tensor-parallel-size "$GPUS_PER_NODE" \
#       --pipeline-parallel-size "${SLURM_NNODES}"

# Tear the cluster down; qdel/GE cleanup is the backstop if this is skipped.
for node in "${NODES[@]}"; do
  srun --nodes=1 --ntasks=1 -w "$node" ray stop || true
done
