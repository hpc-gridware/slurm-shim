# Hugging Face Accelerate (`accelerate launch`) on slurm-shim

[Accelerate](https://huggingface.co/docs/accelerate) launches distributed training
with `accelerate launch`. On SLURM the documented multi-node pattern is the "srun
bootstrap": `srun` runs **one `accelerate launch` per node**, and each launch reads
the SLURM environment to place its ranks. Unlike PyTorch Lightning, Accelerate does
**not** auto-detect SLURM, so the ranks are wired explicitly from the shim's env:

| `accelerate launch` flag | fed from the shim's env |
|---|---|
| `--machine_rank` | `$SLURM_PROCID` (one srun task per node -> node index) |
| `--num_machines` | `$SLURM_NNODES` |
| `--num_processes` | `SLURM_NNODES * GPUS_PER_NODE` |
| `--main_process_ip` | `scontrol show hostnames "$SLURM_JOB_NODELIST" \| head -1` |

`accelerate` then drives `torchrun` (`torch.distributed.elastic`), which forms one
process group across all nodes via a c10d rendezvous at the head node.

## Run it

```bash
# From this directory (the job runs in the submit dir, like SLURM; the training
# script is found via $SLURM_SUBMIT_DIR). `python` must be a Torch + Accelerate
# env on every node -- a shared venv on PATH at submit time is forwarded to the
# job (--export=ALL is the default, like SLURM); or set it up via ENV_SETUP.
sbatch accelerate-multinode.sh
```

- [`accelerate-multinode.sh`](accelerate-multinode.sh) is the srun-bootstrap launch.
  For a different geometry, change `--nodes` / `--gpus-per-node`.
- [`accelerate_train.py`](accelerate_train.py) contains **no SLURM parsing**: it just
  builds an `Accelerator()`, prints the topology Accelerate resolved, and runs one
  `all_reduce` of each rank's index (must equal `0+1+...+(N-1)`) to prove the group
  spans all nodes.

One shim-specific point (handled in the script): it **sources the hook**
(`. /opt/slurm-shim/etc/slurm-shim-source-hook.sh`) so the batch job has
`SLURM_JOB_ID`/`SLURM_NNODES`/`SLURM_JOB_NODELIST` on a site that has not wired
the shim's queue `starter_method`; where it has, the line is a harmless
re-source. Everything else is plain
SLURM behavior: the job runs in the submit dir, `SLURM_SUBMIT_DIR` points at it
(use it for script paths -- the batch script itself is spooled on SLURM too, so
`$0`-relative paths never work there either), and the submit environment is
forwarded (`--export=ALL` default).

## Validated on the OCS test cluster (2026-08-19)

On the 3-node OCS 9.1.4 cluster ([`test/cluster`](../../../test/cluster)), the shim
supplies exactly what `accelerate launch` needs and torch's launcher rendezvous
forms **across all three nodes**: `srun` starts one `accelerate launch` per node,
each with a distinct `SLURM_PROCID` (0/1/2 -> `--machine_rank`), `SLURM_NNODES=3`,
and the head node resolved from `SLURM_JOB_NODELIST`; `torch.distributed.elastic`
then placed global ranks on `ocs-master` / `ocs-worker1` / `ocs-worker2`.

> **CPU caveat (Accelerate, not the shim).** The test cluster is CPU-only, and
> Accelerate's distributed launcher is GPU-oriented: `--multi_gpu` drives
> `torchrun` (NCCL), while its **CPU** path requires `mpirun` (`--mpirun_hostfile`),
> which does not go through `srun`/torchrun. So the collective completes on a **GPU**
> cluster (the pattern above, unchanged), where each rank's `Accelerator()` joins the
> NCCL group. For CPU-only multi-node, use Accelerate's mpirun launcher with the PE's
> native `mpirun` (the shim does not provide PMI for `srun`; see the top-level README).

## GPU notes

- Request GPUs via the `gpu` partition (RSMAP); the shim sets per-rank
  `CUDA_VISIBLE_DEVICES` and `SLURM_GPUS_ON_NODE`, which the script uses for
  `GPUS_PER_NODE`. One `accelerate launch` per node then spawns one process per GPU.
- `MASTER_PORT` defaults to 29500; override if it collides.
