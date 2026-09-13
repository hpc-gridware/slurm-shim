#!/bin/bash
#SBATCH --job-name=train
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=2
#SBATCH --output=slurm-%j.out

echo "[alloc] nodes=$SLURM_NNODES tasks=$SLURM_NTASKS"
echo "[alloc] nodelist=$SLURM_JOB_NODELIST"
echo "[alloc] rendezvous=$MASTER_ADDR:$MASTER_PORT"
srun bash -c 'echo "  rank $SLURM_PROCID/$SLURM_NTASKS on $(hostname)"'
