"""Run every example in definitions.py against the cluster and check the outcome.

The Dagster UI or CLI would do the same; this driver exists so the examples
can be verified in one command. Run it through run_examples.sh, on the submit
host: it asks squeue and sacct itself.
"""
import getpass
import json
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

import dagster as dg

import definitions as ex

HERE = Path(__file__).resolve().parent
DAGSTER = str(Path(sys.executable).parent / "dagster")
SWEEP_DIR = Path(ex.SWEEP_DIR)


def shell(*command):
    """Run a cluster command and return its output; a failing command is an error."""
    done = subprocess.run(command, capture_output=True, text=True)
    if done.returncode != 0:
        raise RuntimeError("%s failed: %s" % (" ".join(command), done.stderr.strip()))
    return done.stdout.strip()


def running_jobs(name):
    """Ids of this user's running cluster jobs with the given job name."""
    out = shell("squeue", "-h", "-u", getpass.getuser(), "-n", name, "-t", "RUNNING", "-o", "%i")
    return set(out.split())


def when_running(name, action=None):
    """Watch in the background for a NEW running job called name.

    Returns a list that receives the job's id once it runs. Jobs of that name
    that are already running are ignored: they belong to somebody else's run,
    or to an earlier one that was interrupted.
    """
    before = running_jobs(name)
    seen = []

    def watch():
        while not seen:
            seen.extend(sorted(running_jobs(name) - before)[:1])
            time.sleep(1)
        if action:
            action(seen[0])

    threading.Thread(target=watch, daemon=True).start()
    return seen


def accounting(job, fields):
    """A finished job's sacct fields. The record appears a moment after the job ends."""
    for _ in range(30):
        row = shell("sacct", "-X", "-n", "-P", "-j", job, "-o", fields + ",State")
        # While the scheduler still removes the job, sacct says RUNNING.
        if row and row.split("|")[-1] not in ("RUNNING", "PENDING"):
            return row.split("|")[:-1]
        time.sleep(1)
    raise RuntimeError("job %s never reached the accounting record" % job)


def materialize(asset, **kwargs):
    """Materialize one asset in this process, recorded in the Dagster instance."""
    return dg.materialize(
        [ex.simulation, ex.sweep_summary, ex.flaky_job, ex.long_job, ex.too_slow],
        selection=[asset],
        resources={"compute": ex.build_compute()},
        instance=dg.DagsterInstance.get(),
        **kwargs,
    )


def metadata_of(result, asset):
    events = result.asset_materializations_for_node(asset.key.to_python_identifier())
    return {key: entry.value for key, entry in events[0].metadata.items()}


def example_sweep():
    """Three partitions, launched together: three cluster jobs side by side."""
    # Results of an earlier sweep would satisfy the checks below on their own.
    for name in ex.TEMPERATURES:
        (SWEEP_DIR / ("%s.json" % name)).unlink(missing_ok=True)

    logs = Path(tempfile.mkdtemp(prefix="dagster-sweep-"))
    runs = []
    for name in ex.TEMPERATURES:
        with open(logs / ("%s.log" % name), "w") as log:
            runs.append(subprocess.Popen(
                [DAGSTER, "asset", "materialize", "-f", str(HERE / "definitions.py"),
                 "--select", "simulation", "--partition", name],
                stdout=log, stderr=subprocess.STDOUT,
            ))
    codes = [run.wait() for run in runs]
    assert codes == [0, 0, 0], "sweep runs exited %s; their logs are in %s" % (codes, logs)

    starts, ends = [], []
    for name in ex.TEMPERATURES:
        result = json.loads((SWEEP_DIR / ("%s.json" % name)).read_text())
        print("simulation[%s]: job %s on %s -> distance %s" % (
            name, result["slurm_job_id"], result["host"], result["distance"]), flush=True)
        start, end = accounting(result["slurm_job_id"], "Start,End")
        starts.append(start)
        ends.append(end)
    # Every job started before the first one ended: they ran side by side.
    assert max(starts) < min(ends), (starts, ends)
    print("[ok] sweep -> 3 partitions, 3 cluster jobs at the same time", flush=True)


def example_summary():
    """A downstream job reads the results and reports an asset check."""
    result = materialize(ex.sweep_summary)
    summary = metadata_of(result, ex.sweep_summary)
    check = result.get_asset_check_evaluations()[0]
    print("sweep_summary: %d runs, farthest is %s (%s), check %s passed=%s" % (
        summary["runs"], summary["farthest"], summary["farthest_distance"],
        check.check_name, check.passed), flush=True)
    assert summary["runs"] == 3 and check.passed
    print("[ok] summary -> results read, asset check passed", flush=True)


def example_retry():
    """A failed job is submitted again by Dagster's retry policy."""
    # A marker left by an interrupted run would let the first attempt succeed.
    (SWEEP_DIR / "flaky.attempted").unlink(missing_ok=True)

    result = materialize(ex.flaky_job)
    jobs = metadata_of(result, ex.flaky_job)
    first, second = jobs["first_attempt_job"], jobs["successful_job"]
    states = accounting(first, "State") + accounting(second, "State")
    print("flaky_job: job %s %s, job %s %s" % (first, states[0], second, states[1]), flush=True)
    assert first != second and states == ["FAILED", "COMPLETED"], (first, second, states)
    print("[ok] retry -> second cluster job succeeded", flush=True)


def example_cancel():
    """Somebody cancels the job on the cluster; the Dagster run fails."""
    cancelled = when_running("dagster_long_job", lambda job: shell("scancel", job))
    result = materialize(ex.long_job, raise_on_error=False)
    assert cancelled, "long_job was never seen running, so nothing was cancelled"
    (state,) = accounting(cancelled[0], "State")
    print("long_job: job %s cancelled with scancel -> sacct says %s" % (
        cancelled[0], state), flush=True)
    assert not result.success and state == "CANCELLED", state
    print("[ok] cancel -> Dagster run failed", flush=True)


def example_time_limit():
    """A job that exceeds its time limit is killed and reported as TIMEOUT."""
    job = when_running("dagster_too_slow")
    result = materialize(ex.too_slow, raise_on_error=False)
    assert job, "too_slow was never seen running"
    (state,) = accounting(job[0], "State")
    print("too_slow: job %s hit its 1 minute limit -> sacct says %s" % (
        job[0], state), flush=True)
    assert not result.success and state == "TIMEOUT", state
    print("[ok] time limit -> Dagster run failed", flush=True)


def main():
    # Create Dagster's history database before starting runs side by side:
    # three processes creating it at the same moment trip over each other.
    instance = dg.DagsterInstance.get()
    instance.get_runs(limit=1)
    instance.all_asset_keys()

    example_sweep()
    example_summary()
    example_retry()
    example_cancel()
    example_time_limit()
    print("[result] DAGSTER-SLURM EXAMPLES OK", flush=True)


if __name__ == "__main__":
    sys.exit(main())
