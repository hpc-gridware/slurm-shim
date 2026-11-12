# dagster-slurm on slurm-shim

[dagster-slurm](https://github.com/ascii-supply-networks/dagster-slurm) runs
[Dagster](https://dagster.io) assets as SLURM jobs. Dagster stays the
orchestrator -- it decides what runs and in which order, streams the logs and
records the results -- while each asset's work happens in a cluster job.

It logs in to the submit host over SSH and drives the scheduler with `sbatch`,
`squeue`, `sacct` and `scancel`. The shim answers those, so the jobs land on
Open Cluster Scheduler. Nothing in this recipe is shim-aware: it is plain
dagster-slurm in `mode="slurm"`.

## Architecture

```
  Dagster + dagster-slurm            submit host                 compute nodes
  (UI, assets, run history)   SSH    slurm-shim                  the job runs the
  "materialize pi_estimate"  ----->  sbatch -> qsub      ---->   payload script
                             <-----  squeue, sacct       <----   logs + results
                                     Open Cluster Scheduler      (dagster-pipes)
```

What has to be installed, and where:

| Where | What | Why |
|---|---|---|
| Cluster | Open Cluster Scheduler and slurm-shim | provide `sbatch`, `squeue`, `sacct`, `scancel`, `srun` |
| Where Dagster runs | Python 3.11 or newer with `dagster-slurm` and `dagster-webserver` | Dagster, the plugin that submits the jobs, and the UI |
| Shared filesystem, same path on every node | a Python environment with `dagster-pipes` | each job uses it to send logs and results back |
| Between the two | SSH key login to the submit host | dagster-slurm runs the scheduler commands over SSH |

Dagster can run on the submit host or on any machine that can SSH to it. In
this recipe it runs on the submit host, and one Python environment serves both
Dagster and the jobs.

From a cluster with slurm-shim to a running pipeline:

1. **Check SSH finds the shim.** `ssh <submit host> which sbatch` must print a
   path.
2. **Create the Python environment** on the shared filesystem with
   [`run.sh`](run.sh) ([what it builds](#two-settings-that-matter)).
3. **Write the assets.** Each asset names a payload script and a job shape
   (partition, nodes, CPUs, time limit). Start from
   [`examples/definitions.py`](examples/definitions.py); the SSH host, user,
   key and the environment path are set in `build_compute()` at the bottom.
   Use partition names from your shim configuration.
4. **Start Dagster** in the directory with your `definitions.py` and
   materialize an asset in the UI
   ([commands and safe access](examples/README.md#7-the-dagster-ui)). That
   submits one cluster job; its logs and results appear in the run view.

To see it work before writing your own assets, run the test pipeline below.

## The scenario

[`pipeline.py`](pipeline.py) defines three assets. Each one is a cluster job.

| Asset | Job shape | What it proves |
|---|---|---|
| `node_census` | 3 nodes | a multi-node job: the payload runs `srun hostname` and reports one host per node |
| `pi_estimate` | 1 node, 4 CPUs | runs after `node_census`; a Monte Carlo estimate of pi on all four CPUs, reported back as metadata |
| `boom` | 1 node | exits with code 3; the Dagster run must fail |

An asset is a few lines. The job shape is the only cluster-specific part:

```python
@dg.asset(deps=[node_census])
def pi_estimate(context: dg.AssetExecutionContext, compute: ComputeResource):
    return compute.run(
        context=context,
        payload_path=str(HERE / "estimate_pi.py"),
        extra_slurm_opts={"partition": "smp", "nodes": 1, "cpus_per_task": 4},
        extra_env={"SAMPLES": "2000000"},
    ).get_results()
```

The payload is an ordinary Python script that reports back through
[Dagster Pipes](https://docs.dagster.io/guides/build/external-pipelines):
[`node_census.py`](node_census.py), [`estimate_pi.py`](estimate_pi.py),
[`boom.py`](boom.py).

## Run it

On the submit host, as a user for whom `ssh <submit host>` works without a
password. To use a specific key, set `SLURM_SSH_KEY` to its path.

```bash
docs/recipes/dagster-slurm/run.sh
```

[`run.sh`](run.sh) creates the Python environment on its first run, then runs
`pipeline.py`.

```
node_census: job 53 ran on ['ocs-master', 'ocs-worker1', 'ocs-worker2']
[ok] multi-node job -> 3 distinct hosts
pi_estimate: job 54 on ocs-worker1 cpus 4 -> 3.141262
[ok] single-node job -> pi with 4 CPUs
[ok] failing job -> Dagster run failed
[result] DAGSTER-SLURM OK
```

## More examples

[`examples/`](examples/README.md) goes further, each case run on the cluster:
a parameter sweep with one job per Dagster partition, passing parameters and
results, a data check reported by the job, retries, a job cancelled on the
cluster, a job that hits its time limit, and the Dagster UI.

## What the shim provides

| dagster-slurm runs | shim behavior |
|---|---|
| `sbatch -J .. -D .. -o .. -e .. -p .. -t .. -c .. --mem=.. -N .. job.sh` | translated to `qsub`; prints `Submitted batch job <N>`; `%j` in the log paths expands to the job id |
| `squeue -h -j <N> -o '%T'` | `PENDING` / `RUNNING` while the job is live, empty once it is gone |
| `sacct -X -n -j <N> -o State%20` | the final state: `COMPLETED`, `FAILED`, `CANCELLED`, `TIMEOUT` |
| `sacct -j <N> -n -P --format=JobID,Elapsed,TotalCPU,MaxRSS,AllocNodes,AllocCPUS,State,ExitCode,...` | one row per job from `qacct`; state and exit code decide success, the rest becomes asset metadata |
| `scancel <N>` | `qdel`; exits 0 silently when the job has already finished, as in SLURM |
| `srun` inside the job | fans out across the job's nodes |

## Two settings that matter

**Use an environment that is already on the cluster.** By default dagster-slurm
packs a [pixi](https://pixi.sh) environment and uploads it for every change.
`pre_deployed_env_path` skips that and uses a directory with this layout:

```
dagster-slurm-env/
  activate.sh      # sourced by the job before the payload
  env/bin/python   # any Python environment containing dagster-pipes
```

`run.sh` builds exactly this: a plain `venv` with `dagster-slurm` and
`dagster-webserver`, plus the one-line `activate.sh`. It must be at the same
path on every node, and so must the Python it was created from: a `venv` links
to that interpreter instead of copying it.

**Match the partition to the job shape.** A multi-node job goes to a partition
whose parallel environment spreads over hosts (`batch` on the test cluster); a
single task that wants several CPUs goes to a `$pe_slots` partition (`smp`), so
its CPUs are on one host. See the
[submitit recipe](../submitit/README.md#multi-node) for the details.

## Validated on the OCS test cluster (2026-11-12)

Run end to end on the 3-node test cluster ([`test/cluster`](../../../test/cluster))
with Open Cluster Scheduler 9.1.6, dagster-slurm 1.24.2, dagster 1.13.29 and
Python 3.11.15. The output above and the details below are from runs on that
cluster.

What dagster-slurm submitted:

```
sbatch -J dagster_node_census -D <run>/node_census -o <run>/node_census/slurm-%j.out -e <run>/node_census/slurm-%j.err -p batch -t 00:10:00 -c 1 --mem=512M -N 3 <run>/node_census/job.sh
sbatch -J dagster_pi_estimate -D <run>/pi_estimate -o <run>/pi_estimate/slurm-%j.out -e <run>/pi_estimate/slurm-%j.err -p smp -t 00:10:00 -c 4 --mem=512M -N 1 <run>/pi_estimate/job.sh
```

- **Dependency order.** `pi_estimate` was submitted after `node_census` finished.
- **Live logs.** The payload's log lines appear in the Dagster log while the job
  runs: `node_census - nodelist: ocs-master,ocs-worker[1-2]`.
- **Results.** The metadata each payload reported is on the asset, next to what
  dagster-slurm reads from `sacct`:

  ```
  node_census {'slurm_job_id': 26, 'nodes': 3, 'hosts': 'ocs-master,ocs-worker1,ocs-worker2',
               'slurm_state': 'COMPLETED', 'slurm_exit_code': 0,
               'elapsed_seconds': 8.0, 'node_hours': 0.01, ...}
  ```

- **Failure.** `boom` exits 3; `sacct` reports it and the run fails with the
  job's stderr in the Dagster log:

  ```
  $ sacct -j 25 -n -P --format=JobID,AllocNodes,State,ExitCode
  25|1|FAILED|3:0
  RuntimeError: Job 25 did not complete successfully. Final state: FAILED.
  ```

Covered, here and in [`examples/`](examples/README.md): per-asset jobs
(`mode="slurm"`) with the `BashLauncher`, partitions, asset checks, retries,
cancellation from the cluster and from the Dagster UI, and time limits. Not
run: session mode, heterogeneous jobs, the Ray and Spark launchers, and pixi
packaging.

## Gotchas

- **Write accounting records immediately.** Grid Engine buffers them, by default
  for 15 seconds. In that time a finished job is in neither `squeue` nor
  `sacct`; dagster-slurm waits 5 seconds, then assumes the job completed, so a
  failed job passes as a success. Set this once, as a manager, in
  `qconf -mconf` under `reporting_params`:

  ```
  accounting_flush_time=00:00:00
  ```

  Verified both ways on the test cluster: with a 15 second flush the failing
  `boom` asset was reported as a success; with the setting above it fails.
- **Use a slurm-shim newer than v0.10.1.** Three answers dagster-slurm relies
  on were fixed for it: `sacct` reports a job that is being cancelled as
  `RUNNING` until its final record exists (v0.10.1 said `COMPLETING`, which
  dagster-slurm reads as success),
  `sacct` answers `AllocNodes` (otherwise `node_hours` is 0), and `scancel` of a
  finished job exits 0 (otherwise every failed job logs a traceback).
- **Exit codes need OCS 9.1.5 or newer.** Older releases record exit status 0
  for these jobs, so a failed payload would look `COMPLETED` to `sacct`. See
  [the write-up](../../solutions/integration-issues/pe-jobs-lose-exit-status-in-accounting.md).
- **SSH finds the shim through the login environment.** dagster-slurm runs each
  command as `bash --noprofile --norc -c ...` over SSH, so `sbatch` must be on
  the `PATH` of a non-interactive SSH session. Step 1 of
  [Architecture](#architecture) checks exactly that.
