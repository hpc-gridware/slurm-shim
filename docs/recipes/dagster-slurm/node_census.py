"""Cluster payload: run `srun hostname` across the allocation and report the hosts."""
import os
import subprocess

from dagster_pipes import open_dagster_pipes

with open_dagster_pipes() as context:
    context.log.info("nodelist: %s" % os.environ["SLURM_JOB_NODELIST"])
    # One task per node of the job.
    output = subprocess.run(
        ["srun", "--ntasks-per-node=1", "hostname"],
        check=True, capture_output=True, text=True,
    ).stdout
    hosts = sorted(output.split())
    context.report_asset_materialization(
        metadata={
            "slurm_job_id": os.environ["SLURM_JOB_ID"],
            "nodes": int(os.environ["SLURM_JOB_NUM_NODES"]),
            "hosts": ",".join(hosts),
        }
    )
