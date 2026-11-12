"""Cluster payload: one run of a toy simulation at a given temperature.

A random walk whose step size grows with the temperature. The result goes to a
JSON file on the shared filesystem (for the summary job) and to Dagster as
metadata (for the UI).
"""
import json
import os
import random
import socket
from pathlib import Path

from dagster_pipes import open_dagster_pipes

with open_dagster_pipes() as context:
    name = context.partition_key
    temperature = context.extras["temperature"]
    context.log.info("simulating %s at temperature %s" % (name, temperature))

    rng = random.Random(name)  # seeded by the partition, so a rerun repeats
    position = 0.0
    for _ in range(200000):
        position += rng.gauss(0.0, temperature)

    result = {
        "name": name,
        "temperature": temperature,
        "distance": round(abs(position), 3),
        "host": socket.gethostname(),
        "slurm_job_id": os.environ["SLURM_JOB_ID"],
    }
    out_dir = Path(os.environ["SWEEP_DIR"])
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / ("%s.json" % name)).write_text(json.dumps(result))

    context.report_asset_materialization(metadata=result)
