"""dagster-slurm smoke test for slurm-shim over Open Cluster Scheduler.

Three Dagster assets, each one a cluster job submitted by dagster-slurm:

  node_census  3 nodes; the payload fans out with srun and reports the hosts
  pi_estimate  1 node, 4 CPUs; runs after node_census and reports its result
  boom         exits non-zero; Dagster must see the failure

Nothing here is shim-aware -- it is plain dagster-slurm in mode="slurm".
"""
import getpass
import os
import socket
import sys
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


@dg.asset
def node_census(context: dg.AssetExecutionContext, compute: ComputeResource):
    """One job across three nodes."""
    return compute.run(
        context=context,
        payload_path=str(HERE / "node_census.py"),
        extra_slurm_opts={"partition": "batch", "nodes": 3, "cpus_per_task": 1},
    ).get_results()


@dg.asset(deps=[node_census])
def pi_estimate(context: dg.AssetExecutionContext, compute: ComputeResource):
    """One task with four CPUs on a single node."""
    return compute.run(
        context=context,
        payload_path=str(HERE / "estimate_pi.py"),
        extra_slurm_opts={"partition": "smp", "nodes": 1, "cpus_per_task": 4},
        extra_env={"SAMPLES": "2000000"},
    ).get_results()


@dg.asset
def boom(context: dg.AssetExecutionContext, compute: ComputeResource):
    """A job that fails."""
    return compute.run(
        context=context,
        payload_path=str(HERE / "boom.py"),
        extra_slurm_opts={"partition": "smp"},
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
        queue=SlurmQueueConfig(time_limit="00:10:00", cpus=1, mem="512M"),
        remote_base=os.environ.get("SLURM_REMOTE_BASE", str(HOME / "dagster-runs")),
    )
    return ComputeResource(
        mode="slurm",
        slurm=slurm,
        default_launcher=BashLauncher(),
        # A Python environment that already exists on the shared filesystem
        # (run.sh creates it), instead of packing and uploading one per run.
        pre_deployed_env_path=os.environ.get(
            "DAGSTER_SLURM_ENV", str(HOME / "dagster-slurm-env")
        ),
    )


def metadata_of(result, asset):
    """The metadata the cluster job reported for an asset, as plain values."""
    events = result.asset_materializations_for_node(asset.key.to_python_identifier())
    return {key: entry.value for key, entry in events[0].metadata.items()}


def main():
    resources = {"compute": build_compute()}

    # 1 + 2. A two-asset pipeline: both jobs run, in dependency order.
    result = dg.materialize([node_census, pi_estimate], resources=resources)
    assert result.success

    census = metadata_of(result, node_census)
    hosts = census["hosts"].split(",")
    print("node_census: job", census["slurm_job_id"], "ran on", hosts, flush=True)
    assert len(set(hosts)) == 3, hosts
    print("[ok] multi-node job -> 3 distinct hosts", flush=True)

    pi = metadata_of(result, pi_estimate)
    print("pi_estimate: job", pi["slurm_job_id"], "on", pi["host"],
          "cpus", pi["cpus"], "->", pi["pi"], flush=True)
    assert abs(pi["pi"] - 3.14159) < 0.01, pi
    assert pi["cpus"] == 4, pi
    print("[ok] single-node job -> pi with 4 CPUs", flush=True)

    # 3. A failing job fails the Dagster run instead of passing or hanging.
    failed = dg.materialize([boom], resources=resources, raise_on_error=False)
    assert not failed.success
    print("[ok] failing job -> Dagster run failed", flush=True)

    print("[result] DAGSTER-SLURM OK", flush=True)


if __name__ == "__main__":
    sys.exit(main())
