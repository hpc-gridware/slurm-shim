# torchrun (multi-node, NCCL) on slurm-shim

The most common way to start PyTorch across nodes. `sbatch` asks for the
GPUs, `srun` starts **one `torchrun` per node**, and `torchrun` starts **one
worker per GPU**. Each worker picks its GPU by `LOCAL_RANK` inside the
`CUDA_VISIBLE_DEVICES` the shim sets from the Grid Engine RSMAP grant.

```bash
sbatch torchrun-gpu.sh
```

- [`torchrun-gpu.sh`](torchrun-gpu.sh) -- 4 nodes x 2 GPUs. The head node comes from
  `scontrol show hostnames`. The rendezvous is `c10d` on the head node, with an id
  and port private to the job and array task.
- [`allreduce_check.py`](allreduce_check.py) -- one NCCL `all_reduce` across all
  workers. Each worker prints the GPU it drives and exits non-zero if the sum is
  wrong.

Swap in your trainer with `TRAIN_SCRIPT=/shared/path/train.py sbatch torchrun-gpu.sh`.
The launch shape is what was verified; the trainer is yours.

## What it shows

- The process group spans every node: the sum is `1 + 2 + ... + world` only if
  every worker took part.
- Every worker drives its own physical GPU. On a 2-GPU node, `local=0` and `local=1`
  pick the first and second device in `cvd`.
- NCCL finds a usable network interface on its own.

It is a launch check, not a training or throughput benchmark.

## Verified on NVIDIA L4

Run on 4 nodes with 2 NVIDIA L4 each (and on 4 nodes with 1 each), on Rocky
Linux 9 with Open Cluster Scheduler 9.1.5, torch 2.6.0+cu124 and NCCL 2.21.5.
Output from the 2-GPU run; host names and device UUIDs are anonymised:

```
HEAD=gpu-node3 NNODES=4
RANK 1/8 local=1 cvd=[GPU-uuid-a,GPU-uuid-b] uuid=uuid-b allreduce=36 expect=36
RANK 0/8 local=0 cvd=[GPU-uuid-a,GPU-uuid-b] uuid=uuid-a allreduce=36 expect=36
RANK 4/8 local=0 cvd=[GPU-uuid-e,GPU-uuid-f] uuid=uuid-e allreduce=36 expect=36
RANK 5/8 local=1 cvd=[GPU-uuid-e,GPU-uuid-f] uuid=uuid-f allreduce=36 expect=36
RANK 2/8 local=0 cvd=[GPU-uuid-g,GPU-uuid-h] uuid=uuid-g allreduce=36 expect=36
RANK 6/8 local=0 cvd=[GPU-uuid-c,GPU-uuid-d] uuid=uuid-c allreduce=36 expect=36
RANK 3/8 local=1 cvd=[GPU-uuid-g,GPU-uuid-h] uuid=uuid-h allreduce=36 expect=36
RANK 7/8 local=1 cvd=[GPU-uuid-c,GPU-uuid-d] uuid=uuid-d allreduce=36 expect=36
```

With `NCCL_DEBUG=INFO`, NCCL reported `NET/Socket : Using [0]eth0:<ip>`: it
picked the node's real interface without `NCCL_SOCKET_IFNAME`.

That run used the same payload and launch line, with these differences, all
kept here on purpose:

| Here | In the verified run | Why |
|---|---|---|
| `srun --ntasks-per-node=1 torchrun ...` | `srun torchrun ...` | That PE derived one task per node from `#SBATCH --ntasks-per-node=1`; a PE with `task_policy: gpu` would start one `torchrun` per GPU. |
| `--rdzv_id "$SLURM_JOB_ID-$task"` | a fixed id | Unique per job and per array task. All tasks of an array job share `SLURM_JOB_ID`. |
| `--rdzv_endpoint "$head:$port"`: the shim's `MASTER_PORT` if exported, else derived from the job and task id in 20000-29999 | `"$head:29500"` | A fixed port lets two jobs that share their first node meet in one rendezvous. |
| stops early if `SLURM_JOB_NODELIST` is unset | - | Without the SLURM environment, the endpoint would be empty and `torchrun` would fail with an unclear error. |

## Adapt it

- **GPUs per node:** change `--gpus-per-node` and `--nproc_per_node` together. With
  1 GPU per node, both are `1`.
- **Nodes:** `sbatch --nodes=2 torchrun-gpu.sh` (command-line flags override `#SBATCH`).
- **One node, no srun:** the batch script itself gets `CUDA_VISIBLE_DEVICES` for
  its node (shim v0.6.0 and later), so a plain `python train.py` under `sbatch`
  sees only its granted GPUs. This was seen on L4, including a CUDA allocation in the
  batch script.

## Site requirements

- A `gpu` partition backed by an RSMAP complex. See
  [GPU jobs](../README.md#gpu-jobs) for the prerequisites.
- `torchrun` (and the same torch) on `PATH` on every node.
- **Network:** the rendezvous port lies in the shim's 20000-29999 range. NCCL
  itself connects every worker to every other worker, on ports it picks, which
  the shim's ranges do not cover. On a firewalled cluster, allow TCP between the
  compute nodes ([Networking](../../../README.md#networking-firewalled-clusters)).
  Set `NCCL_SOCKET_IFNAME` only if NCCL picks the wrong interface.
