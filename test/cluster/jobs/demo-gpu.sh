#!/bin/bash
# GPU demo: submitted directly with `qsub -pe make 2 -l gpu=2`. The PE
# start_proc_args hook runs the fabricator, which discovers the granted RSMAP
# devices via qstat and publishes them as CUDA_VISIBLE_DEVICES.
#
# Both binding models are shown. Like SLURM, the shim binds a task to a subset of
# the grant only when asked: without a bind flag every rank sees the node's whole
# grant (what JAX and LOCAL_RANK-indexing frameworks need), while
# --gpus-per-task=1 gives each rank its own device.


echo "[alloc] SLURM_GPUS_ON_NODE=$SLURM_GPUS_ON_NODE SLURM_JOB_GPUS=$SLURM_JOB_GPUS"
echo "[alloc] nodelist=$SLURM_JOB_NODELIST"
echo "[default binding: whole grant visible to every rank]"
srun -n 2 bash -c 'echo "  rank $SLURM_PROCID on $(hostname): CUDA_VISIBLE_DEVICES=[${CUDA_VISIBLE_DEVICES-unset}]"'
echo "[--gpus-per-task=1: one device per rank]"
srun -n 2 --gpus-per-task=1 bash -c 'echo "  rank $SLURM_PROCID on $(hostname): CUDA_VISIBLE_DEVICES=[${CUDA_VISIBLE_DEVICES-unset}]"'
