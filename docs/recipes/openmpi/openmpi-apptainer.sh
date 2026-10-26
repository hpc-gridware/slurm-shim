#!/bin/bash
#SBATCH --job-name=openmpi-apptainer
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=4
#SBATCH --output=openmpi-apptainer-%j.out
# OpenMPI ranks inside an Apptainer image ("hybrid" model): the host's mpirun
# and its daemons start the ranks, and each rank runs in the container. Build
# openmpi.sif from openmpi.def first. The host's OpenMPI must be the image's
# release series (4.1), on PATH, at the same path on every node.
set -e
cd "$SLURM_SUBMIT_DIR"
IMAGE=${IMAGE:-$PWD/openmpi.sif}
# Ranks in an unprivileged container live in another user namespace, where
# OpenMPI's default shared-memory copy (CMA) is not allowed; say so up front
# instead of a warning per node.
export OMPI_MCA_btl_vader_single_copy_mechanism=none

echo "== mpirun, ranks in the container"
mpirun apptainer -q exec "$IMAGE" /opt/hello_mpi | sort -t= -k2 -n

echo "== srun --mpi=pmix, ranks in the container"
srun --mpi=pmix apptainer -q exec "$IMAGE" /opt/hello_mpi | sort -t= -k2 -n
