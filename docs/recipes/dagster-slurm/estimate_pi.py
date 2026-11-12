"""Cluster payload: Monte Carlo estimate of pi on every CPU the job was granted."""
import os
import random
import socket
from multiprocessing import Pool

from dagster_pipes import open_dagster_pipes


def hits_in_circle(samples):
    rng = random.Random()
    return sum(rng.random() ** 2 + rng.random() ** 2 <= 1.0 for _ in range(samples))


if __name__ == "__main__":
    with open_dagster_pipes() as context:
        cpus = int(os.environ["SLURM_CPUS_PER_TASK"])
        samples = int(os.environ.get("SAMPLES", "1000000"))
        context.log.info("sampling %d points on %d CPUs" % (samples, cpus))
        with Pool(cpus) as pool:
            hits = sum(pool.map(hits_in_circle, [samples // cpus] * cpus))
        context.report_asset_materialization(
            metadata={
                "slurm_job_id": os.environ["SLURM_JOB_ID"],
                "host": socket.gethostname(),
                "cpus": cpus,
                "pi": 4.0 * hits / (samples // cpus * cpus),
            }
        )
