"""Dagster definitions for the dagster-slurm examples on slurm-shim.

Load this file with the Dagster UI or CLI:

    dagster dev -f definitions.py
    dagster asset materialize -f definitions.py --select simulation --partition cold

Every asset below runs as one cluster job. See README.md for what each shows.
"""
import getpass
import os
import socket
from pathlib import Path

import dagster as dg
from dagster_slurm import (
    BashLauncher,
    ComputeResource,
    SlurmQueueConfig,
    SlurmResource,
    SSHConnectionResource,
)

HERE = Path(__file__).resolve().parent
HOME = Path.home()

# Jobs exchange files here, so it must be on the shared filesystem.
SWEEP_DIR = os.environ.get("SWEEP_DIR", str(HOME / "dagster-sweep"))

# One partition per parameter set. Dagster tracks each one separately.
TEMPERATURES = {"cold": 0.5, "warm": 1.0, "hot": 2.0}
sweep = dg.StaticPartitionsDefinition(list(TEMPERATURES))


# --- A parameter sweep, one cluster job per partition ------------------------
@dg.asset(partitions_def=sweep)
def simulation(context: dg.AssetExecutionContext, compute: ComputeResource):
    """Run one simulation for the partition's temperature."""
    return compute.run(
        context=context,
        payload_path=str(HERE / "simulate.py"),
        extra_slurm_opts={"partition": "smp", "nodes": 1},
        # extras travel to the payload as structured data (context.extras).
        extras={"temperature": TEMPERATURES[context.partition_key]},
        # extra_env travels as plain environment variables.
        extra_env={"SWEEP_DIR": SWEEP_DIR},
    ).get_results()


# --- A job that reads the sweep and reports a data check ---------------------
@dg.asset(
    deps=[simulation],
    check_specs=[dg.AssetCheckSpec("all_runs_present", asset="sweep_summary")],
)
def sweep_summary(context: dg.AssetExecutionContext, compute: ComputeResource):
    """Compare the sweep's results and check that none is missing."""
    return compute.run(
        context=context,
        payload_path=str(HERE / "summarize.py"),
        extra_slurm_opts={"partition": "smp"},
        extras={"expected": list(TEMPERATURES)},
        extra_env={"SWEEP_DIR": SWEEP_DIR},
    ).get_results()


# --- Retry a job that fails --------------------------------------------------
@dg.asset(retry_policy=dg.RetryPolicy(max_retries=2, delay=1))
def flaky_job(context: dg.AssetExecutionContext, compute: ComputeResource):
    """Fails on its first attempt; Dagster submits it again."""
    return compute.run(
        context=context,
        payload_path=str(HERE / "flaky.py"),
        extra_slurm_opts={"partition": "smp"},
        extra_env={"SWEEP_DIR": SWEEP_DIR},
    ).get_results()


# --- A long job, to cancel from outside --------------------------------------
@dg.asset
def long_job(context: dg.AssetExecutionContext, compute: ComputeResource):
    """Sleeps for ten minutes unless somebody cancels it."""
    return compute.run(
        context=context,
        payload_path=str(HERE / "sleeper.py"),
        extra_slurm_opts={"partition": "smp"},
        extra_env={"SLEEP_SECONDS": "600"},
    ).get_results()


# --- A job that runs into its time limit -------------------------------------
@dg.asset
def too_slow(context: dg.AssetExecutionContext, compute: ComputeResource):
    """Asks for one minute and needs ten."""
    return compute.run(
        context=context,
        payload_path=str(HERE / "sleeper.py"),
        extra_slurm_opts={"partition": "smp", "time_limit": "00:01:00"},
        extra_env={"SLEEP_SECONDS": "600"},
    ).get_results()


def build_compute():
    """dagster-slurm reaches the submit host over SSH and runs sbatch there."""
    ssh = SSHConnectionResource(
        host=os.environ.get("SLURM_SSH_HOST", socket.gethostname()),
        user=os.environ.get("SLURM_SSH_USER", getpass.getuser()),
        # Without SLURM_SSH_KEY, ssh decides as it does on the command line:
        # ~/.ssh/config, the agent, the default key files.
        key_path=os.environ.get("SLURM_SSH_KEY"),
        # Trust the host key on first contact. Fine for the host you are on;
        # use "strict" for a remote host whose key is already in known_hosts.
        host_key_checking="accept-new",
    )
    slurm = SlurmResource(
        ssh=ssh,
        # Defaults for every job; an asset overrides them with extra_slurm_opts.
        # 15 minutes leaves room for long_job, which sleeps for ten.
        queue=SlurmQueueConfig(time_limit="00:15:00", cpus=1, mem="512M"),
        remote_base=os.environ.get("SLURM_REMOTE_BASE", str(HOME / "dagster-runs")),
    )
    return ComputeResource(
        mode="slurm",
        slurm=slurm,
        default_launcher=BashLauncher(),
        pre_deployed_env_path=os.environ.get(
            "DAGSTER_SLURM_ENV", str(HOME / "dagster-slurm-env")
        ),
    )


defs = dg.Definitions(
    assets=[simulation, sweep_summary, flaky_job, long_job, too_slow],
    resources={"compute": build_compute()},
)
