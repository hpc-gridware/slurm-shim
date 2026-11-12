"""Cluster payload: sleep, for the cancel and time-limit examples."""
import os
import time

from dagster_pipes import open_dagster_pipes

with open_dagster_pipes() as context:
    seconds = int(os.environ["SLEEP_SECONDS"])
    context.log.info("job %s sleeping %d seconds" % (os.environ["SLURM_JOB_ID"], seconds))
    time.sleep(seconds)
    context.report_asset_materialization(metadata={"slept_seconds": seconds})
