# submitit on slurm-shim

[submitit](https://github.com/facebookincubator/submitit) (Meta) submits Python
functions to SLURM and reads their results back. It shells out to exactly five
binaries -- `sbatch`, `sacct`, `scancel`, `scontrol requeue`, `srun` -- so the
shim just has to answer those the way submitit expects. Nothing in this recipe is
shim-aware: it is plain submitit against `cluster="slurm"`.

## What the shim provides

| submitit needs | shim behavior |
|---|---|
| `sbatch <script>` printing `job <N>` | translates `#SBATCH` directives to `qsub` (partition -> `-q`/`-pe`, `--array` -> `qsub -t`/`-tc`, `--time`/`--mem`/`--gpus` -> `-l`, `--dependency` -> `-hold_jid`) |
| `sacct -o JobID,State,NodeList --parsable2 -j <N>` | answered from `qstat` (live) + `qacct` (finished); 0-based array elements; unknown ids omitted so submitit keeps polling |
| `srun --unbuffered --output <f> --error <f>` | runs the task; expands `%A/%a/%j/%t` in the output path to the 0-based files submitit reads |
| `scancel <id>` / `scontrol requeue <id>` | `qdel` / `qmod -rj` |

**Array indexing.** SLURM/submitit arrays are 0-based; Grid Engine tasks are
1-based. `sbatch --array=0-{n-1}` submits a dense `qsub -t 1-n` and records the
SLURM origin/step in the job env; the shim's env fabricator maps each GE task back
to its 0-based SLURM index, so `SLURM_ARRAY_TASK_ID`, the result/log filenames,
and `sacct` all agree on `<N>_<0-based>`.

**`done()` rides on the result pickle.** submitit decides success/failure from a
result pickle on the shared filesystem, not from exit codes. `sacct` is the
backstop for a job that dies *without* writing a pickle: it must report a terminal
state so `result()` raises instead of hanging.

## Run it

```bash
docs/recipes/submitit/run.sh
```

`run.sh` bootstraps a shared venv (`pip install submitit`) and runs
[`submitit_smoke.py`](submitit_smoke.py), which submits a single function, a
`map_array`, and a failing function.

The one shim-specific line: submitit writes its own batch script, so -- unlike the
lightning/ray recipes -- it does not source the shim hook, and `srun` would not be
on `PATH` in the batch shell. The driver adds it via submitit's own
`slurm_setup` parameter:

```python
ex.update_parameters(slurm_setup=[". /opt/slurm-shim/etc/slurm-shim-source-hook.sh"])
```

(Override the hook path with `SHIM_HOOK`, or set it cluster-wide so no per-job
line is needed.)

## Validated on the OCS test cluster (2026-08-19)

Confirmed end to end on the 3-node OCS 9.1.4 test cluster
([`test/cluster`](../../../test/cluster)) with submitit 1.5.4:

```
single job id: 35
[ok] single submit -> 5
array ids: ['36_0', '36_1', '36_2', '36_3'] -> [11, 22, 33, 44]
[ok] map_array -> [11, 22, 33, 44]
[ok] failure surfaced: FailedJobError
[result] SUBMITIT OK
```

- **Single submit + result** works (the result pickle path).
- **`map_array`** returns every value, and the job ids are **0-based**
  (`36_0..36_3`) -- proof the array-index mapping lines up across submit, env,
  filenames, and `sacct`.
- **A failing function surfaces as an exception** (`FailedJobError`) instead of
  hanging.

Raw `sacct`, matching submitit's parser exactly:

```
$ sacct -o JobID,State,NodeList --parsable2 -j 35
JobID|State|NodeList
35|COMPLETED|
$ sacct -o JobID,State,NodeList --parsable2 -j 36
JobID|State|NodeList
36_0|COMPLETED|
36_1|COMPLETED|
36_2|COMPLETED|
36_3|COMPLETED|
$ sacct -o JobID,State,NodeList --parsable2 -j 999999   # unknown -> header only
JobID|State|NodeList
```

A live job reports `RUNNING` with its node (`40|RUNNING|ocs-master`); after a
`scancel` it reports a terminal `CANCELLED` (`40|CANCELLED|`), so `result()` never
hangs. (submitit decides success/failure from the result pickle regardless; the
terminal `sacct` state is the backstop for a job that dies without a pickle.)

### The official `mnist.py`

submitit's own [`docs/mnist.py`](https://github.com/facebookincubator/submitit/blob/main/docs/mnist.py)
(train a logistic-regression MNIST classifier, `cpus_per_task=4`, `mem_gb=1`) also
runs on the shim and trains to completion:

```
Scheduled SlurmJob<job_id=54, task_id=0, state="RUNNING">.
*** Entering stage 'Data Loading' ***  ... 'Data Cleaning' ... 'Model Training'
Test score with L1 penalty: 0.8305
Job completed successfully
Finished training. Final score: 0.8305.
```

Three things it needs, none of which are shim bugs:

1. **The `slurm_setup` hook line** (above) -- provides `SLURM_JOB_ID`/`SLURM_NTASKS`.
   Without it the worker aborts with `KeyError: 'SLURM_JOB_ID'`.
2. **A single-node PE** for the `cpus_per_task=4` request. The default `make` PE is
   `$round_robin` and would scatter the 4 slots across nodes (breaking `srun`); use
   a partition backed by a `$pe_slots` PE with the `node` task policy -- the test
   cluster's [`smp` partition](../../../test/cluster/config.yaml)
   (`slurm_partition="smp"`). This is a deployment choice, not a code change: the
   shim faithfully requests the slots; the PE decides placement.
3. **sklearn-version compat in the example itself** -- current sklearn removed the
   `LogisticRegression(multi_class=...)` arg and `fetch_openml` returns pandas, so
   the upstream example needs `multi_class` dropped and `X.numpy()` -> `np.asarray(X)`.

### Checkpoint / preemption / requeue

submitit's `Checkpointable` jobs (and its `job._interrupt()`) **work on the shim**,
including the official `mnist.py` preemption demo -- a job interrupted mid-training
checkpoints, requeues, resumes from the checkpoint, and returns its result:

```
Scheduled SlurmJob<job_id=59, state="RUNNING">.
preempting SlurmJob<job_id=59, state="RUNNING"> after 12s
Finished training. Final score: 0.8387.
```

How it maps to Grid Engine (GE has no "send an arbitrary signal to a running job",
but it does have `-notify` + reschedule):

| submitit | shim -> GE |
|---|---|
| `#SBATCH --signal=USR2@<n>` | `qsub -notify -r y` -- GE sends **SIGUSR2** before a kill/reschedule (submitit's default preempt signal) and the job is rerunnable; plus `-l s_rt=h_rt-<n>` as an early SIGUSR1 warning near the walltime |
| `job._interrupt()` -> `scancel <id> --signal ...` | `qmod -rj <id>` (reschedule) -- delivers SIGUSR2 to the running job, then terminates and restarts it |
| checkpoint handler -> `scontrol requeue <id>` | `qmod -rj <id>` -- GE reschedules the rerunnable job (`RESTARTED=1`); submitit resumes from the checkpoint pickle |

While a job is between the reschedule and its restart, GE's `qacct` shows
`failed 25 : rescheduling`, which `sacct` maps to `REQUEUED` (non-terminal), so
submitit keeps polling until the resumed run finishes.

Caveats: the pre-kill grace is the queue-level `notify` time (default 60s), not the
per-job `@<n>`, so the checkpoint must finish within it; and because `-notify`
delays every kill by that window, a plain `scancel <id>` (a real cancel) of such a
job takes up to `notify` seconds to take effect (it still wins over the handler's
requeue -- the job is cancelled, not resurrected).

## Gotchas

- **Array cancel.** `scancel <N>` cancels the whole array; `scancel N_k` cancels
  the 0-based element k (mapped to GE task k+1, consistent with the ids `sacct`
  reports). Native 1-based GE arrays are off by one through `scancel` -- cancel
  those by the whole array or with `qdel` directly.
- **Memory requests.** `--mem`/`slurm_mem` maps to the site's memory complex
  (`memory_complex`, default `h_vmem`). `h_vmem` is enforced as virtual address
  space and can kill CUDA/JVM jobs that reserve large virtual memory; on GPU
  clusters set `memory_complex` to a resident/reservation complex (`mem_free` /
  `h_rss`).
