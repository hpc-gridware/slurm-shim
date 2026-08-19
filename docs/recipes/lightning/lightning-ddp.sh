#!/bin/bash
#SBATCH --job-name=lightning-ddp
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=1
#SBATCH --output=lightning-%j.out
# Multi-node PyTorch Lightning DDP. One rank per node. `srun` fabricates the
# per-rank SLURM_* env (PROCID/NODEID/LOCALID/NTASKS) and MASTER_ADDR; Lightning's
# SLURMEnvironment reads them and wires up DDP itself -- no torchrun, no glue.
# CPU/gloo here (functionality, not throughput); for GPU set the accelerator and
# request the gpu partition.
#
# `python` must be a Torch + Lightning environment on every node (shared venv,
# container image, or module). Override with PYTHON_BIN.
set -uo pipefail
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh

PY="${PYTHON_BIN:-python3}"
srun "$PY" "$(dirname "$0")/lightning_ddp.py"
