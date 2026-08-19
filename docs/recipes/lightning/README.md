# PyTorch Lightning (multi-node DDP) on slurm-shim

This is the recipe that most directly exercises **the reason the shim exists**:
a framework whose *own code* recognizes SLURM. Unlike Ray (which is
scheduler-agnostic and only needs "one process per node"), PyTorch Lightning
ships a first-class [`SLURMEnvironment`](https://lightning.ai/docs/pytorch/stable/api/lightning.pytorch.plugins.environments.SLURMEnvironment.html)
plugin. When it detects a SLURM job it reads the environment and wires up DDP
itself -- no `torchrun`, no rendezvous flags, no wrapper glue:

| Lightning reads | shim fabricates |
|---|---|
| `SLURM_PROCID` | global rank of this task (per rank, by `srun`) |
| `SLURM_NTASKS` | world size |
| `SLURM_NODEID` | node rank |
| `SLURM_LOCALID` | local rank on the node |
| `SLURM_JOB_NODELIST` / `MASTER_ADDR` | rendezvous host (rank 0) |

So the whole integration is: `srun python train.py`. Lightning does the rest
from the shim's env.

## Run it

```bash
sbatch lightning-ddp.sh          # one DDP rank per node
```

- [`lightning-ddp.sh`](lightning-ddp.sh) is just `srun python lightning_ddp.py`
  across the allocation. `python` must be a **Torch + Lightning** environment
  present on every node (a shared venv on a shared filesystem, a container image,
  or an environment module); point at it with `PYTHON_BIN`.
- [`lightning_ddp.py`](lightning_ddp.py) contains **no SLURM parsing**. It builds a
  `Trainer(strategy="ddp", accelerator="cpu", devices=1, num_nodes=$SLURM_NNODES)`,
  and a callback prints the topology Lightning resolved and runs one collective
  (`all_reduce` of each rank's global rank -> must equal `0+1+...+(N-1)`) to prove
  the process group actually spans all nodes.

For GPU DDP, swap `accelerator="gpu"`, request the `gpu` partition, and let the
shim's RSMAP grant set `CUDA_VISIBLE_DEVICES` per rank.

## Shim requirements

- `export_master_addr: true` in the shim config (so `MASTER_ADDR`/`MASTER_PORT`
  are published for the rendezvous).
- `srun` supplies the per-rank `SLURM_PROCID` / `SLURM_NODEID` / `SLURM_LOCALID`
  and `SLURM_NTASKS` (Table A + Table B) -- the same values validated by the
  `test/e2e` srun fan-out.

## Validated on the OCS test cluster (2026-08-19)

Confirmed end to end on the 3-node OCS 9.1.4 test cluster
([`test/cluster`](../../../test/cluster)), CPU/gloo. Torch + Lightning were put in
a shared venv on the shared home; then `sbatch lightning-ddp.sh`:

```
[rank 0/3] host=ocs-master   env=SLURMEnvironment SLURM_PROCID=0 MASTER_ADDR=ocs-master allreduce_sum=3 expected=3
[rank 1/3] host=ocs-worker2  env=SLURMEnvironment SLURM_PROCID=1 MASTER_ADDR=ocs-master allreduce_sum=3 expected=3
[rank 2/3] host=ocs-worker1  env=SLURMEnvironment SLURM_PROCID=2 MASTER_ADDR=ocs-master allreduce_sum=3 expected=3
[result] LIGHTNING SLURM DDP OK
```

Lightning's `SLURMEnvironment` activated on every rank (the framework's own SLURM
integration, reading the shim's env -- not a wrapper). The per-rank
`SLURM_PROCID`/`SLURM_NODEID` mapped to distinct hosts, `MASTER_ADDR` pointed at
the rank-0 host, and the `all_reduce` of the ranks equalled `0+1+2` on all three
-- so the DDP process group genuinely spanned all nodes over the shim's rendezvous.
The job exited on its own in ~20 s with no stray processes. This is the most
direct proof of what the shim is for: a framework that natively speaks SLURM works
unchanged on OCS.

> **Gotcha (not shim-related):** the gloo process-group teardown can hang after a
> one-shot job (a known Torch/gloo issue). `lightning_ddp.py` calls `os._exit(0)`
> once `fit` returns to sidestep it; drop that for long-lived services. Either way
> the shim's `scancel`/`qdel` reliably reaps a stuck step.
