"""One NCCL all-reduce across every torchrun worker of the job.

Each worker contributes its global rank + 1, so the result equals
1 + 2 + ... + world only if every worker took part, across every node. Each
worker also reports the physical GPU it drives, so two workers on one device
show up as a repeated uuid. GPU only: without CUDA this fails at
init_process_group("nccl") instead of passing on the CPU.
"""
import os
import sys

import torch
import torch.distributed as dist

dist.init_process_group("nccl")
rank, world = dist.get_rank(), dist.get_world_size()
local = int(os.environ["LOCAL_RANK"])
torch.cuda.set_device(local)

# Each rank contributes its own value, so the sum is only correct if every rank
# actually took part -- a collective that silently excluded a rank fails here.
t = torch.ones(1, device="cuda") * (rank + 1)
dist.all_reduce(t)
expect = world * (world + 1) // 2

props = torch.cuda.get_device_properties(torch.cuda.current_device())
cvd = os.environ.get("CUDA_VISIBLE_DEVICES", "unset")
print(
    f"RANK {rank}/{world} local={local} cvd=[{cvd}] "
    f"uuid={props.uuid} allreduce={int(t.item())} expect={expect}",
    flush=True,
)
dist.destroy_process_group()
sys.exit(0 if int(t.item()) == expect else 1)
