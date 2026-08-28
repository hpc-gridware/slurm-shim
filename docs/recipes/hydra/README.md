# Hydra on slurm-shim

[Hydra](https://hydra.cc) is the config layer under a large share of ML research
code. Its [submitit launcher](https://hydra.cc/docs/plugins/submitit_launcher/)
turns `--multirun` into one cluster job per sweep combination — so if Hydra runs,
every Hydra app runs.

It works on the shim with **no code changes and one config line**: Hydra drives
[submitit](../submitit/), which the shim already supports end to end.

## Run it

```bash
pip install hydra-core hydra-submitit-launcher

# Sweep on the cluster: 3 configs -> 3 jobs
python train.py --multirun lr=0.1,0.01,0.001
```

- [`train.py`](train.py) — a normal `@hydra.main` app. No SLURM parsing; the only
  scheduler call is a submitit job-id lookup, purely to show where the job landed.
- [`config.yaml`](config.yaml) — selects the launcher and its resources.

The whole cluster integration is in `config.yaml`:

```yaml
defaults:
  - _self_
  - override hydra/launcher: submitit_slurm     # send --multirun jobs to the cluster

hydra:
  launcher:
    partition: smp            # a $pe_slots partition (see the trap below)
    timeout_min: 5
    nodes: 1
    tasks_per_node: 1
    cpus_per_task: 1
    setup:                    # the one shim-specific line
      - . /opt/slurm-shim/etc/slurm-shim-source-hook.sh
```

`setup` is Hydra's passthrough to submitit's `slurm_setup`. It is needed because
Hydra writes its own batch script, so it must source the hook to get
`SLURM_JOB_ID` / `SLURM_NTASKS` and `srun` on `PATH`. Set the hook cluster-wide and
you can drop even that line.

Everything else is stock Hydra — override launcher settings per run:

```bash
python train.py --multirun lr=0.1,0.01 hydra.launcher.partition=batch
python train.py --multirun lr=0.1,0.01 epochs=1,5      # 2-D sweep -> 4 jobs
```

## Validated on the OCS test cluster (2026-08-20, re-run 2026-08-28)

3-node cluster ([`test/cluster`](../../../test/cluster)), Hydra 1.3.5 + submitit
1.5.4, first on OCS 9.1.4 and re-run unchanged on **OCS 9.1.5** (same three losses,
same one-task-per-node spread). `--multirun lr=0.1,0.01,0.001` became a 3-task
array, each task on a **different node**:

```
[2026-08-20 18:35:36,248][HYDRA] Submitit 'slurm' sweep output dir : multirun/2026-08-20/18-35-36
[2026-08-20 18:35:36,249][HYDRA] 	#0 : lr=0.1
[2026-08-20 18:35:36,250][HYDRA] 	#1 : lr=0.01
[2026-08-20 18:35:36,252][HYDRA] 	#2 : lr=0.001
```

and the per-task logs under `.submitit/`:

```
[job 169_0] host=ocs-master  lr=0.1   epochs=3
[job 169_0] loss=0.008100
[job 169_1] host=ocs-worker1 lr=0.01  epochs=3
[job 169_1] loss=0.000000
[job 169_2] host=ocs-worker2 lr=0.001 epochs=3
[job 169_2] loss=0.000081
```

A 2-D sweep (`lr=0.1,0.01 epochs=1,5`) produced the expected 4 jobs, and CLI
overrides of `hydra.launcher.*` took effect.

Note the job ids are **0-based** (`169_0..169_2`), matching what Hydra and submitit
expect, even though Grid Engine numbers array tasks from 1 — the shim maps between
the two.

## The one trap: cpus_per_task and the partition

Raising `cpus_per_task` on a **round-robin** partition silently runs your function
more than once.

`hydra.launcher.cpus_per_task=2` with the round-robin `batch` partition asks for 2
slots, the PE spreads them over 2 nodes, and a slot-per-task policy then starts 2
tasks — so the sweep entry executes **twice**, once per node:

```
[job 133_0] host=ocs-worker1 lr=0.1 epochs=1
[job 133_0] host=ocs-worker2 lr=0.1 epochs=1
```

Same job id, two hosts: the sweep entry ran twice.

Use a partition backed by a `$pe_slots` PE with the `node` task policy — the test
cluster's `smp`, which this recipe defaults to. Verified: `cpus_per_task=4` on
`smp` runs each job exactly once.

| Your job shape | Partition to use |
|---|---|
| one task, several CPUs (the common Hydra case) | `$pe_slots` + `node` policy (`smp`) |
| genuinely multi-node (`nodes: 2+`) | round-robin (`batch`) |

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `KeyError: 'SLURM_JOB_ID'` in the job | the batch script never sourced the shim hook | add the `setup:` line above |
| `sbatch: warning: Grid Engine cannot express %a` on every sweep | expected: GE's `$TASK_ID` is a dense 1..N, so the batch-level file is renamed | nothing to do -- the per-task logs Hydra reads are written by `srun` and are unaffected |
| A task fails before Hydra logs anything | the traceback happens *before* submitit's `srun`, so it is not in a `%A_%a` log | look at the batch-level file `.submitit/slurm-<jobid>.<n>.out` (see below) |
| Each sweep entry runs twice (or N times) | `cpus_per_task > 1` on a round-robin partition | use a `$pe_slots` partition (see above) |
| Jobs submit but stay queued | asked for more slots than the partition can place | check `qstat -j <id>`; `qalter -w p <id>` says whether it can ever schedule |
| Sweep runs locally, not on the cluster | the launcher override is missing | confirm `hydra/launcher: submitit_slurm` is in `defaults:`, and that `hydra-submitit-launcher` is installed |

### Where the logs are

Two sets of files, both under the sweep's `.submitit/` directory:

- `<jobid>_<n>/<jobid>_<n>_0_log.out` — the per-task logs Hydra and submitit read.
  Written by `srun`, indexed the way Hydra expects (0-based).
- `slurm-<jobid>.<n>.out` — the batch-level stream: anything printed *before*
  submitit starts, which is where a bad `setup:` line or a missing interpreter
  shows up. `sbatch` prints a warning naming this file at submit time.

The second name exists because Grid Engine numbers array tasks from 1 and offers
no equivalent of SLURM's `%a`, so the shim substitutes a name it *can* expand
rather than one pointing at the wrong task. Note the `<n>` there is the GE task
number (1-based), not the Hydra sweep index.

## What else you get free

Because the launcher is submitit, the rest of the [submitit recipe](../submitit/)
applies unchanged: array throttling (`hydra.launcher.array_parallelism`), job
requeue/checkpointing, and `sacct`-based state tracking. Hydra sweeper plugins
(Optuna, Nevergrad, Ax) drive the same launcher, so they submit to the cluster the
same way — the sweeper picks the configs, the launcher places them.
