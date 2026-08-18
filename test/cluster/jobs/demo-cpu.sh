#!/bin/bash
# CPU demo: submitted through the shim's `sbatch` (partition -> queue + PE +
# slots). The PE start_proc_args hook fabricates the SLURM_* environment; this
# script sources it, then `srun` fans ranks out across the allocation over
# qrsh -inherit tight integration.
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=2

. /opt/slurm-shim/etc/slurm-shim-source-hook.sh

echo "[alloc] SLURM_NNODES=$SLURM_NNODES SLURM_NTASKS=$SLURM_NTASKS"
echo "[alloc] nodelist=$SLURM_JOB_NODELIST master=$MASTER_ADDR:$MASTER_PORT"
srun bash -c 'echo "  rank $SLURM_PROCID/$SLURM_NTASKS local=$SLURM_LOCALID node=$SLURM_NODEID on $(hostname)"'
