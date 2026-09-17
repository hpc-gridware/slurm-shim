# slurm-shim recipes

Runnable patterns for popular AI stacks that need a little glue to go from
"Partial" to "Ready" on the shim. Each recipe is a submittable job script plus a
short README explaining the shim-specific bits.

Submit any of these with the shim's `sbatch` (or plain `qsub`) inside a Grid
Engine PE job. They assume the shim's symlinks (`srun`, `sbatch`, `squeue`,
`scancel`, `scontrol`, `sinfo`) are on `PATH`.

| Recipe | Stack | Pattern | GPU (verified on L4) |
|--------|-------|---------|----------------------|
| [`gpu-check/`](gpu-check/) | any GPU site | one line per rank: the GPUs granted vs. the GPUs reachable | ✅ [`gpu-check.sh`](gpu-check/gpu-check.sh) |
| [`torchrun/`](torchrun/) | PyTorch (`torchrun`, NCCL, multi-node) | `srun --ntasks-per-node=1 torchrun`, one worker per GPU | ✅ [`torchrun-gpu.sh`](torchrun/torchrun-gpu.sh) |
| [`lightning/`](lightning/) | PyTorch Lightning (multi-node DDP) | `srun python train.py`; Lightning's `SLURMEnvironment` self-configures from the shim's env | |
| [`deepspeed/`](deepspeed/) | DeepSpeed (and any torch.distributed trainer) | `srun --ntasks-per-node=1 torchrun` per node | |
| [`ray/`](ray/) | Ray (Train / Tune / Serve, vLLM multi-node) | `srun` bootstraps a Ray head + workers | |
| [`clearml/`](clearml/) | clearml-agent (SLURM mode) | site `#SBATCH` template -> `sbatch` -> `squeue` polling | |
| [`submitit/`](submitit/) | submitit (submit Python functions, arrays) | `sbatch`/`sacct`/`srun`; 0-based arrays, result-pickle tracking | |
| [`accelerate/`](accelerate/) | HF Accelerate (`accelerate launch`, multi-node) | `srun` runs one `accelerate launch` per node; `SLURM_PROCID` -> `--machine_rank` | |
| [`hydra/`](hydra/) | Hydra (`--multirun` sweeps) | `hydra/launcher: submitit_slurm` -> one cluster job per sweep config; no code changes | |
| [`jax/`](jax/) | JAX (multi-process, multi-node) | `srun python train.py`; `jax.distributed.initialize()` auto-detects from five `SLURM_*` vars -- no PMI, no glue | ✅ [`jax-gpu-check.sh`](jax/jax-gpu-check.sh) |
| [`flax/`](flax/) | Flax (data-parallel training, multi-node) | same auto-detect, then one global `jax.sharding` mesh over every process's devices; gradients all-reduce across nodes | |

A ✅ marks a GPU job that ran on real NVIDIA L4 nodes. Every other validation
run recorded in these recipes was on the CPU test cluster.

## GPU jobs

The GPU examples above ran on 4 nodes with 1 and with 2 NVIDIA L4 each, on
Rocky Linux 9 with Open Cluster Scheduler 9.1.5, torch 2.6.0+cu124,
NCCL 2.21.5 and JAX 0.4.30.

**What the site needs:**

- **A GPU partition** backed by an RSMAP complex with `consumable HOST`
  (`qconf -sc`). A per-slot `YES` complex multiplies the request by the slot
  count, and the job never starts.
- **One RSMAP id per device**, preferably the device UUID
  ([Configuration](../../README.md#configuration)).
- **A memory complex that does not cap address space.** It must never be
  `h_vmem`, which kills CUDA at start-up ([Memory requests](../../README.md#memory-requests)).
- **The same Python environment** at the same path on every node.
- **Network:** the shim's port ranges, plus TCP between compute nodes for NCCL
  ([Networking](../../README.md#networking-firewalled-clusters)).

**How to request GPUs:** use `#SBATCH --gpus-per-node=N`, which means N per node.
How many GPUs each task sees is covered in
[Pick the right GPU-visibility model](#2-pick-the-right-gpu-visibility-model).

**Where to start:** run [`gpu-check/`](gpu-check/) first, then [`torchrun/`](torchrun/).

**The source line:** the GPU scripts do not source the shim's hook. They rely
on the queue `starter_method` that `slurm-shim install --apply` sets up. On a
site without one, add `. "$SGE_ROOT/slurm-shim/etc/slurm-shim-source-hook.sh"`
as the first command, or enable `wrapper_mode`.

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
how you shape the step. These recipes name `CUDA_VISIBLE_DEVICES` throughout; on a
site configured with `gpu.vendor: amd` the same per-rank mask is written as
`ROCR_VISIBLE_DEVICES` instead -- exactly one of the two is ever set, and the other
vendor's variables are removed from the rank environment so an inherited value
cannot layer on top. Nothing else below changes.

- **One task per node (recommended for torchrun).** `srun --ntasks-per-node=1`
  gives that single task the node's **whole** granted GPU set
  (`CUDA_VISIBLE_DEVICES=0,1,...`). A `torchrun --nproc_per_node=$SLURM_GPUS_ON_NODE`
  under it forks workers that index devices by `LOCAL_RANK` -- the classic model.

- **One task per GPU, whole set visible (JAX, and anything selecting by local
  rank).** `srun --ntasks-per-node=8` with GPUs requested **per node** starts eight
  tasks that each still see all eight devices; the framework picks its own by
  `SLURM_LOCALID`/`LOCAL_RANK`. This is SLURM's default -- no binding unless you
  ask -- and it is what [`jax/`](jax/) requires.

- **One task per GPU, masked.** `srun --ntasks-per-node=8 --gpus-per-task=1` (or
  `--gpu-bind=per_task`) masks each task to **one** GPU (`CUDA_VISIBLE_DEVICES=<one id>`,
  seen as `cuda:0`). Your code must then use device 0, **not** `LOCAL_RANK`. Do not
  use this for JAX: it indexes by `SLURM_LOCALID` and ranks above 0 would fail.

Most recipes here use the first model because it matches how torchrun-based stacks
(DeepSpeed, Accelerate, Megatron, most trainers) expect the world to look. A site
that wants the shim to split a node's grant among tasks by default (its behavior
before SLURM parity) can set `gpu.bind: per-task` in the config.
