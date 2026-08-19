"""Minimal multi-node PyTorch Lightning DDP job.

Nothing here parses SLURM_*: Lightning's SLURMEnvironment auto-detects the job
and reads SLURM_PROCID/NTASKS/NODEID/LOCALID (+ the rendezvous address) that the
shim fabricated. We just print the resolved topology and run one collective to
prove the process group actually spans all nodes. CPU/gloo, tiny -- functionality
only.
"""
import os
import socket

import torch
import torch.distributed as dist
import pytorch_lightning as pl


class Tiny(pl.LightningModule):
    def __init__(self):
        super().__init__()
        self.net = torch.nn.Linear(4, 1)

    def training_step(self, batch, _):
        x, y = batch
        return torch.nn.functional.mse_loss(self.net(x), y)

    def configure_optimizers(self):
        return torch.optim.SGD(self.parameters(), lr=0.01)


class Report(pl.Callback):
    def on_train_start(self, trainer, pl_module):
        env = type(trainer.strategy.cluster_environment).__name__
        gr, ws = trainer.global_rank, trainer.world_size
        # Sum every rank's global_rank across the whole group. If the group truly
        # spans all N ranks/nodes, the sum is 0+1+...+(N-1) on every rank.
        t = torch.tensor([float(gr)])
        dist.all_reduce(t)
        expected = ws * (ws - 1) / 2
        print(
            f"[rank {gr}/{ws}] host={socket.gethostname()} env={env} "
            f"node_rank={trainer.node_rank} local_rank={trainer.local_rank} "
            f"SLURM_PROCID={os.environ.get('SLURM_PROCID')} "
            f"SLURMD_NODENAME={os.environ.get('SLURMD_NODENAME')} "
            f"MASTER_ADDR={os.environ.get('MASTER_ADDR')} "
            f"allreduce_sum={t.item():.0f} expected={expected:.0f}",
            flush=True,
        )
        if gr == 0:
            ok = (
                env == "SLURMEnvironment"
                and ws == int(os.environ.get("SLURM_NTASKS", "0"))
                and ws >= 3
                and abs(t.item() - expected) < 1e-6
            )
            print("[result] LIGHTNING SLURM DDP OK" if ok else "[result] LIGHTNING SLURM DDP FAILED", flush=True)


def main():
    torch.manual_seed(0)
    # Exactly one full batch per rank (drop_last) so every rank runs the same
    # number of steps -- otherwise uneven DDP step counts deadlock the all-reduce.
    ws = int(os.environ.get("SLURM_NTASKS", "1"))
    ds = torch.utils.data.TensorDataset(torch.randn(8 * ws, 4), torch.randn(8 * ws, 1))
    dl = torch.utils.data.DataLoader(ds, batch_size=8, drop_last=True)
    trainer = pl.Trainer(
        accelerator="cpu",
        devices=1,
        num_nodes=int(os.environ.get("SLURM_NNODES", "1")),
        strategy="ddp",
        max_epochs=1,
        logger=False,
        enable_checkpointing=False,
        enable_progress_bar=False,
        callbacks=[Report()],
    )
    trainer.fit(Tiny(), dl)
    # gloo's process-group teardown can hang on some hosts; we have proven the
    # multi-node group works, so exit hard once fit returns instead of waiting on
    # destruction. (Harmless for a one-shot job; drop it for long-lived services.)
    import sys

    sys.stdout.flush()
    sys.stderr.flush()
    os._exit(0)


if __name__ == "__main__":
    main()
