"""Minimal multi-node Hugging Face Accelerate job.

Nothing here parses SLURM_*: `accelerate launch` (in accelerate-multinode.sh)
reads the shim's SLURM_* env to place the ranks, and Accelerator() picks up the
resulting distributed setup. We print the resolved topology and run one collective
to prove the process group actually spans all nodes. CPU/gloo, tiny -- functionality
only.
"""
import socket

import torch
from accelerate import Accelerator


def main():
    acc = Accelerator()
    # Sum every rank's global index across the whole group. If the group truly
    # spans all N processes, the sum is 0+1+...+(N-1) on every rank.
    idx = torch.tensor([float(acc.process_index)], device=acc.device)
    total = acc.reduce(idx, reduction="sum")
    expected = acc.num_processes * (acc.num_processes - 1) / 2
    print(
        f"[rank {acc.process_index}/{acc.num_processes}] host={socket.gethostname()} "
        f"device={acc.device} local_rank={acc.local_process_index} "
        f"allreduce_sum={total.item():.0f} expected={expected:.0f}",
        flush=True,
    )
    if acc.is_main_process:
        ok = acc.num_processes >= 3 and abs(total.item() - expected) < 1e-6
        print("[result] ACCELERATE MULTINODE OK" if ok else "[result] ACCELERATE FAILED", flush=True)


if __name__ == "__main__":
    main()
