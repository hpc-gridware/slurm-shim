"""Cluster payload: fails the first time it runs, succeeds the second time."""
import os
import sys
from pathlib import Path

from dagster_pipes import open_dagster_pipes

marker = Path(os.environ["SWEEP_DIR"]) / "flaky.attempted"
marker.parent.mkdir(parents=True, exist_ok=True)

if not marker.exists():
    marker.write_text(os.environ["SLURM_JOB_ID"])
    print("first attempt: simulated node trouble", file=sys.stderr)
    sys.exit(1)

with open_dagster_pipes() as context:
    context.report_asset_materialization(
        metadata={
            "first_attempt_job": marker.read_text(),
            "successful_job": os.environ["SLURM_JOB_ID"],
        }
    )
marker.unlink()
