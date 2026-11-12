# dagster-slurm examples

Seven things people do with Dagster on a cluster, each one run on the OCS test
cluster through slurm-shim. Start with the [recipe](../README.md) if you have
not run it yet: it explains the setup and creates the Python environment these
examples use.

| # | Example | What you learn |
|---|---|---|
| 1 | [Parameter sweep](#1-parameter-sweep-one-job-per-partition) | one cluster job per Dagster partition, running side by side |
| 2 | [Parameters in, results out](#2-parameters-in-results-out) | how values reach the job and how results come back |
| 3 | [A data check from the job](#3-a-data-check-from-the-job) | a downstream job reads the sweep and reports pass or fail |
| 4 | [Retry a failed job](#4-retry-a-failed-job) | Dagster submits the job again |
| 5 | [Somebody cancels the job](#5-somebody-cancels-the-job) | `scancel` on the cluster fails the Dagster run |
| 6 | [The job runs out of time](#6-the-job-runs-out-of-time) | the time limit is enforced and reported as `TIMEOUT` |
| 7 | [The Dagster UI](#7-the-dagster-ui) | launch a run in the browser, terminate it, and the job is cancelled |

All assets are in [`definitions.py`](definitions.py). Each asset is one cluster
job, and each payload is a short Python script next to it.

## Run them

```bash
docs/recipes/dagster-slurm/run.sh                    # once: creates the environment
docs/recipes/dagster-slurm/examples/run_examples.sh  # examples 1 to 6, about 2 minutes
```

```
simulation[cold]: job 58 on ocs-master -> distance 85.121
simulation[warm]: job 56 on ocs-worker1 -> distance 654.505
simulation[hot]: job 57 on ocs-worker2 -> distance 218.72
[ok] sweep -> 3 partitions, 3 cluster jobs at the same time
sweep_summary: 3 runs, farthest is warm (654.505), check all_runs_present passed=True
[ok] summary -> results read, asset check passed
flaky_job: job 60 FAILED, job 61 COMPLETED
[ok] retry -> second cluster job succeeded
long_job: job 62 cancelled with scancel -> sacct says CANCELLED
[ok] cancel -> Dagster run failed
too_slow: job 63 hit its 1 minute limit -> sacct says TIMEOUT
[ok] time limit -> Dagster run failed
[result] DAGSTER-SLURM EXAMPLES OK
```

[`run_examples.py`](run_examples.py) is only a test driver: it starts the runs
and checks the outcome. In daily use you start runs from the Dagster UI or the
`dagster` command line, as shown below.

## 1. Parameter sweep: one job per partition

A Dagster *partition* is one slice of an asset that is tracked on its own. Here
each partition is one parameter set, and each one becomes one cluster job.

```python
TEMPERATURES = {"cold": 0.5, "warm": 1.0, "hot": 2.0}
sweep = dg.StaticPartitionsDefinition(list(TEMPERATURES))

@dg.asset(partitions_def=sweep)
def simulation(context: dg.AssetExecutionContext, compute: ComputeResource):
    return compute.run(
        context=context,
        payload_path=str(HERE / "simulate.py"),
        extra_slurm_opts={"partition": "smp", "nodes": 1},
        extras={"temperature": TEMPERATURES[context.partition_key]},
        extra_env={"SWEEP_DIR": SWEEP_DIR},
    ).get_results()
```

Start one partition from the command line:

```bash
dagster asset materialize -f definitions.py --select simulation --partition cold
```

Start all three at once and the cluster runs them at the same time, each on
the node the scheduler picks. In the UI this is a *backfill*.

```
$ sacct -n -P -j 56,57,58 -o JobID,JobName,State,Start,End,NodeList
56|dagster_simulation__partition=warm|COMPLETED|2026-11-12T08:24:23|2026-11-12T08:24:29|ocs-worker1
57|dagster_simulation__partition=hot|COMPLETED|2026-11-12T08:24:23|2026-11-12T08:24:30|ocs-worker2
58|dagster_simulation__partition=cold|COMPLETED|2026-11-12T08:24:23|2026-11-12T08:24:29|ocs-master
```

The partition is part of the job name, so `squeue` shows which job belongs to
which parameter set.

## 2. Parameters in, results out

Three channels, all visible in example 1 and in [`simulate.py`](simulate.py):

| Direction | How | In the payload |
|---|---|---|
| Dagster -> job, structured | `extras={"temperature": 0.5}` | `context.extras["temperature"]` |
| Dagster -> job, plain | `extra_env={"SWEEP_DIR": "/shared/sweep"}` | `os.environ["SWEEP_DIR"]` |
| job -> Dagster | `context.report_asset_materialization(metadata={...})` | shown on the asset in the UI |

The payload also sees the cluster's own environment (`SLURM_JOB_ID`,
`SLURM_CPUS_PER_TASK`, `SLURM_JOB_NODELIST`, ...) and the partition it runs for
(`context.partition_key`).

Larger results do not travel through Dagster. The job writes them to the shared
filesystem, and the next job reads them from there:

```python
(out_dir / ("%s.json" % name)).write_text(json.dumps(result))   # simulate.py
results[name] = json.loads(path.read_text())                    # summarize.py
```

## 3. A data check from the job

`sweep_summary` runs after the sweep. Its job reads every result file, reports
the farthest run, and reports an *asset check*: a pass or fail that Dagster
shows next to the asset.

```python
@dg.asset(
    deps=[simulation],
    check_specs=[dg.AssetCheckSpec("all_runs_present", asset="sweep_summary")],
)
def sweep_summary(context: dg.AssetExecutionContext, compute: ComputeResource):
    ...   # compute.run(...) as in example 1, with payload summarize.py
```

`deps` makes it run after the sweep; `check_specs` declares the check the job
is going to report.

The check itself is three lines in [`summarize.py`](summarize.py):

```python
context.report_asset_check(
    check_name="all_runs_present",
    passed=not missing,
    metadata={"missing": ",".join(missing)},
)
```

## 4. Retry a failed job

Nodes fail. A retry policy on the asset makes Dagster submit the job again:

```python
@dg.asset(retry_policy=dg.RetryPolicy(max_retries=2, delay=1))
def flaky_job(context: dg.AssetExecutionContext, compute: ComputeResource):
    ...
```

[`flaky.py`](flaky.py) exits 1 on its first attempt. The cluster shows two
jobs for the one asset, and the Dagster run succeeds:

```
$ sacct -n -P -j 60,61 -o JobID,JobName,State,ExitCode
60|dagster_flaky_job|FAILED|1:0
61|dagster_flaky_job|COMPLETED|0:0
```

```
flaky_job - Execution failed: Job 60 did not complete successfully. Final state: FAILED.
flaky_job - STEP_UP_FOR_RETRY - Execution of step "flaky_job" failed and has requested a retry in 1 seconds.
flaky_job - STEP_RESTARTED - Started re-execution (attempt # 2) of step "flaky_job".
```

## 5. Somebody cancels the job

An administrator, or the user in another terminal, cancels the cluster job
while Dagster is waiting for it:

```bash
scancel 62
```

Dagster notices and fails the run instead of waiting or reporting success:

```
RuntimeError: Job 62 did not complete successfully. Final parent state: RUNNING (exit ).
```

Dagster asks while the scheduler is still removing the job, hence `RUNNING`. A
moment later the accounting record names the reason:

```
$ sacct -X -n -P -j 62 -o State
CANCELLED
```

## 6. The job runs out of time

The time limit is a job option like any other. `too_slow` asks for one minute
and sleeps for ten:

```python
extra_slurm_opts={"partition": "smp", "time_limit": "00:01:00"},
```

The scheduler kills the job at the limit, and both sides agree on why:

```
$ sacct -n -P -j 63 -o JobID,JobName,State,Elapsed,Timelimit
63|dagster_too_slow|TIMEOUT|00:01:01|00:01:00
RuntimeError: Job 63 did not complete successfully. Final state: TIMEOUT.
```

## 7. The Dagster UI

```bash
cd docs/recipes/dagster-slurm/examples
export DAGSTER_HOME=$HOME/dagster-home        # run history; run_examples.sh uses the same
$HOME/dagster-slurm-env/env/bin/dagster dev -f definitions.py -p 3000
```

The Dagster UI has no login: whoever can reach it can submit cluster jobs as
you and terminate your runs. `dagster dev` therefore listens on `localhost`
only, and you reach it from your own machine through an SSH tunnel:

```bash
ssh -L 3000:localhost:3000 <submit host>
```

Do not start it with `-h 0.0.0.0` on a shared submit host.

Open `http://localhost:3000`. The five assets are listed with their
history, metadata and checks. **Materialize** on an asset submits its cluster
job; the job's log lines appear in the run view while it runs.

**Terminate** on a running run cancels the cluster job. Verified by launching
`long_job` through the web server and terminating the run:

```
run ffbcb1f1 STARTED   -> cluster job 52 RUNNING
terminate              -> run CANCELED
$ sacct -X -n -P -j 52 -o State
CANCELLED
```

## Where to go from here

- **Bigger jobs.** `extra_slurm_opts` takes `nodes`, `cpus_per_task`, `mem`,
  `time_limit`, `partition` and `gpus_per_node`. The [recipe](../README.md)
  has a multi-node asset that uses `srun` inside the job.
- **Defaults for every job.** `SlurmQueueConfig` in `build_compute()` sets the
  partition, time limit, CPUs and memory an asset gets when it does not ask.
- **A different submit host.** `definitions.py` reads its connection from the
  environment. For Dagster on another machine, set all of these; the three
  paths are paths on the cluster, and their defaults are derived from the
  local home directory:

  | Variable | Meaning |
  |---|---|
  | `SLURM_SSH_HOST`, `SLURM_SSH_USER` | the submit host and the login there |
  | `SLURM_SSH_KEY` | a specific key; unset, `ssh` uses its own configuration |
  | `DAGSTER_SLURM_ENV` | the Python environment on the cluster |
  | `SLURM_REMOTE_BASE` | where dagster-slurm keeps its run directories |
  | `SWEEP_DIR` | where the example jobs exchange files |

  dagster-slurm uploads the payload scripts for each run; the Python
  environment must already be on the cluster. In these runs Dagster ran on
  the submit host itself and connected to it over SSH. `run_examples.py`
  only works there, because it calls `squeue`, `sacct` and `scancel` itself.
