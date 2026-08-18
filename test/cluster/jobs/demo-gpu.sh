#!/bin/bash
# GPU demo: submitted directly with `qsub -pe make 2 -l gpu=2` (the shim's sbatch
# does not translate GPU requests yet). The PE start_proc_args hook still runs the
# fabricator, which discovers the granted RSMAP devices via qstat and hands each
# rank its own CUDA_VISIBLE_DEVICES.

. /opt/slurm-shim/etc/slurm-shim-source-hook.sh

echo "[alloc] SLURM_GPUS_ON_NODE=$SLURM_GPUS_ON_NODE SLURM_JOB_GPUS=$SLURM_JOB_GPUS"
echo "[alloc] nodelist=$SLURM_JOB_NODELIST"
srun -n 2 bash -c 'echo "  rank $SLURM_PROCID on $(hostname): CUDA_VISIBLE_DEVICES=[${CUDA_VISIBLE_DEVICES-unset}]"'
