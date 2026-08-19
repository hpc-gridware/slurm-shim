# slurm-shim recipes

Runnable patterns for popular AI stacks that need a little glue to go from
"Partial" to "Ready" on the shim. Each recipe is a submittable job script plus a
short README explaining the shim-specific bits.

Submit any of these with the shim's `sbatch` (or plain `qsub`) inside a Grid
Engine PE job. They assume the shim's symlinks (`srun`, `sbatch`, `squeue`,
`scancel`, `scontrol`, `sinfo`) are on `PATH`.

| Recipe | Stack | Pattern |
|--------|-------|---------|
| [`lightning/`](lightning/) | PyTorch Lightning (multi-node DDP) | `srun python train.py`; Lightning's `SLURMEnvironment` self-configures from the shim's env |
| [`deepspeed/`](deepspeed/) | DeepSpeed (and any torch.distributed trainer) | `srun --ntasks-per-node=1 torchrun` per node |
| [`ray/`](ray/) | Ray (Train / Tune / Serve, vLLM multi-node) | `srun` bootstraps a Ray head + workers |
| [`clearml/`](clearml/) | clearml-agent (SLURM mode) | site `#SBATCH` template -> `sbatch` -> `squeue` polling |
| [`submitit/`](submitit/) | submitit (submit Python functions, arrays) | `sbatch`/`sacct`/`srun`; 0-based arrays, result-pickle tracking |
| [`accelerate/`](accelerate/) | HF Accelerate (`accelerate launch`, multi-node) | `srun` runs one `accelerate launch` per node; `SLURM_PROCID` -> `--machine_rank` |

## The one thing to understand first

The shim fabricates the SLURM environment (`SLURM_PROCID`, `SLURM_LOCALID`,
`SLURM_NODEID`, `SLURM_NNODES`, `SLURM_NTASKS`, `SLURM_JOB_NODELIST`,
`SLURM_GPUS_ON_NODE`, per-rank `CUDA_VISIBLE_DEVICES`, ...) and provides the
`srun`/`sbatch`/`scontrol` CLIs over GE tight integration. It does **not** provide
PMI/PMIx: `srun --mpi=pmix` hard-errors, so MPI-bootstrapped launches must use the
PE's native `mpirun` instead. Everything here bootstraps from the SLURM
environment or `srun`, never from MPI.

## Two things every multi-node recipe needs

### 1. Derive `MASTER_ADDR` yourself

`MASTER_ADDR`/`MASTER_PORT` are **off by default** (`export_master_addr: false`).
Derive the rendezvous host from the nodelist -- `scontrol show hostnames` prints
one host per line, master first:

```bash
MASTER_ADDR=$(scontrol show hostnames | head -n1)   # reads $SLURM_JOB_NODELIST
MASTER_PORT=${MASTER_PORT:-29500}
```

(Or set `export_master_addr: true` in the shim config and let it compute a
collision-free port; note PyTorch Lightning ignores it and derives its own.)

### 2. Pick the right GPU-visibility model

The shim assigns GPUs per `srun` rank, so how many GPUs a process sees depends on
how you shape the step:

- **One task per node (recommended for torchrun).** `srun --ntasks-per-node=1`
  gives that single task the node's **whole** granted GPU set
  (`CUDA_VISIBLE_DEVICES=0,1,...`). A `torchrun --nproc_per_node=$SLURM_GPUS_ON_NODE`
  under it forks workers that index devices by `LOCAL_RANK` -- the classic model.

- **One task per GPU.** `srun --ntasks-per-node=8 --gpus-per-task=1` masks each
  task to **one** GPU (`CUDA_VISIBLE_DEVICES=<one id>`, seen as `cuda:0`). Your
  code must then use device 0, **not** `LOCAL_RANK`, to select the GPU.

The recipes here use the first model because it matches how torchrun-based stacks
(DeepSpeed, Accelerate, Megatron, most trainers) expect the world to look.
