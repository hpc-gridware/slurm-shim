# DeepSpeed on slurm-shim

Moves DeepSpeed from **Partial** to **Ready**. DeepSpeed integrates with
`torch.distributed`; the only question on a scheduler is who sets
`RANK`/`LOCAL_RANK`/`WORLD_SIZE`/`MASTER_ADDR`. This recipe lets **torchrun** do
it, which sidesteps DeepSpeed's MPI/hostfile launcher and its
silent-single-process failure mode.

## Recommended: torchrun per node

[`deepspeed-multinode.sh`](deepspeed-multinode.sh) runs **one `srun` task per
node**, and each task launches a `torchrun` that forks one worker per GPU:

```bash
sbatch deepspeed-multinode.sh          # -> Submitted batch job <id>
```

Why this shape:

- `--ntasks-per-node=1` -> the node's single task is granted the node's **whole**
  GPU set, so `torchrun --nproc_per_node=$SLURM_GPUS_ON_NODE` sees all of them and
  its workers index by `LOCAL_RANK` (the standard torchrun model).
- torchrun sets every `torch.distributed` variable, so
  `deepspeed.init_distributed()` (or `deepspeed.initialize(...)`) just works. This
  is the important bit: calling DeepSpeed's distributed init **without** the rank
  variables set is what degrades to a one-process "MPI singleton" (a
  silent-wrong-result hazard). torchrun guarantees they are set.
- `MASTER_ADDR` is derived from `scontrol show hostnames | head -n1` because the
  shim keeps it off by default.

Your `train.py` should do the normal DeepSpeed thing; no shim-specific code:

```python
import deepspeed, torch
deepspeed.init_distributed()                       # reads torchrun's env
local_rank = int(os.environ["LOCAL_RANK"])
torch.cuda.set_device(local_rank)                  # all node GPUs visible here
model_engine, optimizer, _, _ = deepspeed.initialize(
    args=args, model=model, model_parameters=params)
```

## Alternative: DeepSpeed's native `--launcher slurm`

DeepSpeed's `SlurmRunner` wants an MPI-style hostfile (`host slots=N`) plus a
`SLURM_PROCID -> RANK` mapping. It is more moving parts and relies on inter-node
rsh, so prefer the torchrun path above. If you must use it, generate the hostfile
from the shim's environment:

```bash
scontrol show hostnames | while read -r h; do
  echo "$h slots=${SLURM_GPUS_ON_NODE:-8}"
done > deepspeed_hostfile
deepspeed --hostfile deepspeed_hostfile --launcher slurm train.py --deepspeed --deepspeed_config ds_config.json
```

and set the rank glue in your entrypoint before `init_distributed()`:

```bash
export RANK="$SLURM_PROCID"
export LOCAL_RANK="$SLURM_LOCALID"
export WORLD_SIZE="$SLURM_NTASKS"
export MASTER_ADDR="$(scontrol show hostnames | head -n1)"
export MASTER_PORT="${MASTER_PORT:-29500}"
```

## Requirements

- A `gpu` RSMAP complex granting GPUs on each node (the shim's discovery turns
  the grant into per-rank `CUDA_VISIBLE_DEVICES`).
- NCCL reachability between nodes (already required by GE builtin-IJS qrsh
  back-connections, so no new networking).
