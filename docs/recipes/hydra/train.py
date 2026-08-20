"""A Hydra app that runs its sweep on the cluster.

Nothing here is scheduler-aware: `@hydra.main` reads config.yaml, and Hydra's
submitit launcher turns each sweep combination into its own cluster job. The
returned value is what Hydra sweepers (Optuna, Nevergrad, ...) optimize over.
"""
import socket

import hydra
from omegaconf import DictConfig


@hydra.main(version_base=None, config_path=".", config_name="config")
def main(cfg: DictConfig) -> float:
    # Only to show where the job landed; a real app would not need this.
    import submitit

    try:
        job_id = submitit.JobEnvironment().job_id
    except RuntimeError:
        job_id = "local"

    print(
        f"[job {job_id}] host={socket.gethostname()} "
        f"lr={cfg.lr} epochs={cfg.epochs}",
        flush=True,
    )

    # Stand-in for training: a parabola minimized at lr=0.01.
    loss = (cfg.lr - 0.01) ** 2
    print(f"[job {job_id}] loss={loss:.6f}", flush=True)
    return loss


if __name__ == "__main__":
    main()
