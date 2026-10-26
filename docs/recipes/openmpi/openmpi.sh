#!/bin/bash
#SBATCH --job-name=openmpi
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=4
#SBATCH --output=openmpi-%j.out
# OpenMPI the two ways SLURM sites run it: mpirun in the batch script, and
# srun --mpi=pmix. Both start one rank per task (here 12 on 3 nodes).
# Needs OpenMPI 4.1 or newer at the same path on every node, with mpicc and
# mpirun on PATH, and this directory on a shared filesystem.
set -e
cd "$SLURM_SUBMIT_DIR"
mpicc -O2 -o hello_mpi hello_mpi.c

echo "== mpirun"
mpirun ./hello_mpi | sort -t= -k2 -n

echo "== srun --mpi=pmix"
srun --mpi=pmix ./hello_mpi | sort -t= -k2 -n
