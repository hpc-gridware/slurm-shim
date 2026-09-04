---
title: control_slaves TRUE lost a job's exit_status in qacct, so sacct could not see a failed job -- fixed in OCS 9.1.5
type: integration-issue
component: sacct, accounting
status: resolved-upstream
date: 2026-08-20
resolved: 2026-08-28
affected: OCS/GCS 9.1.4 and earlier (reproduced on 9.0.10 and 9.1.4)
fixed_in: Open Cluster Scheduler 9.1.5 (250826-0734)
tags: [sacct, qacct, exit-code, parallel-environment, control-slaves, upstream, resolved]
---

# control_slaves TRUE lost a job's exit_status in qacct

**Resolved upstream in OCS 9.1.5.** A job that exits non-zero under a tightly
integrated PE is now reported `FAILED` / `<code>:0` by `sacct`. The shim needed no
change: its mapping was already correct and started reporting the right answer the
moment the accounting record carried the status. Everything below is kept because
the affected releases are still deployed.

## Symptom (OCS 9.1.4 and earlier)

`sacct` reported a job that exited non-zero as `COMPLETED` with `ExitCode 0:0`:

```
$ sbatch --wrap='exit 3'
Submitted batch job 228
$ sacct -j 228 -o JobID,State,ExitCode --parsable2
JobID|State|ExitCode
228|COMPLETED|0:0
```

The shim's mapping was not at fault: `qacct` itself recorded `exit_status 0`.

## Root cause

A job that ran under a PE with **`control_slaves TRUE`** lost its script's exit
status in the accounting record. It was that one setting -- not the presence of a
PE and not the shim: with `control_slaves FALSE` the status was recorded
correctly.

Isolated with plain `qsub`, a plain two-line script, and a PE carrying no shim
wiring at all (`start_proc_args NONE`), changing exactly one attribute at a time:

```bash
printf '#!/bin/bash\nexit 3\n' > e3.sh; chmod +x e3.sh
qsub -terse -q all.q [-pe <pe> 1] -N ctl -o /dev/null -j y e3.sh
```

Build `cleanpe` by editing a `qconf -sp make` dump rather than writing one from
scratch: `qconf -Ap` rejects a file that omits any required attribute
(`error: required attribute "ign_sreq_on_mhost" is missing`), and the full set is
longer than the one the manual page leads you to write. Copying the live PE and
substituting the attributes under test also keeps the comparison honest -- every
row differs only in what is named.

`exit_status` as recorded by `qacct`, measured on 9.0.10 and 9.1.5 on the same
day with the same script:

| PE | `start_proc_args` | `control_slaves` | 9.0.10 | **9.1.5** |
|----|-------------------|------------------|--------|-----------|
| none | - | - | 3 | **3** |
| `cleanpe` | NONE | FALSE | 3 | **3** |
| `cleanpe` | NONE | **TRUE** | **0** | **3** |
| `cleanpe` | NONE | TRUE, `job_is_first_task FALSE` | **0** | **3** |
| `cleanpe` | NONE | TRUE, `accounting_summary FALSE` | **0** | **3** |
| `make` (the shim's), 1 slot | `slurm-shim-env` | TRUE | **0** | **3** |
| `make` (the shim's), 3 slots | `slurm-shim-env` | TRUE | **0** | **3** |

Rows 2 and 3 differ in `control_slaves` alone, which pinned the cause. Row 3 ran
no shim code whatsoever, which ruled the shim out: it was stock OCS behavior.
Rows 4 and 5 vary `job_is_first_task` and `accounting_summary` on top of it and
change nothing, so neither is involved.

9.1.4 was measured the same way when this was first written and matched the
9.0.10 column on every row it covered (no PE, `cleanpe` FALSE/TRUE, and the shim's
`make` PE), which is why the fault is attributed to the whole 9.0/9.1 line up to
9.1.4 rather than to one release.

**The shim could not avoid it.** `control_slaves TRUE` is what lets `sge_execd`
own the `qrsh -inherit` slave tasks -- it is the basis of the tight integration
`srun` is built on -- so the shim's PEs require it. Any Grid Engine site running
tightly integrated parallel jobs had the same gap, shim or not.

**Per-task accounting did not recover it either.** On 9.1.4 with
`accounting_summary FALSE` on the `make` PE, a 3-task job whose script ends in
`exit 3` writes three records -- one per `qrsh -inherit` task plus the master
(`pe_taskid NONE`) -- and *every* one of them, the master included, carried
`failed 0 / exit_status 0`. So switching a cluster to per-task accounting was not
a workaround: the status was lost at the point the master record was written, not
in the summarization.

On 9.1.5 the same probe shows the fix landing exactly where it should -- the
master record carries the status, the slave task records carry their own (the
`qrsh -inherit` tasks really did exit 0):

```
pe_taskid 1.ocs-worker2   failed 0   exit_status 0
pe_taskid 1.ocs-master    failed 0   exit_status 0
pe_taskid NONE            failed 0   exit_status 3     <- the master, correct
```

`sacct` reports `FAILED` / `3:0` for that job. Two things per-task accounting
still changes, both handled in `JobAccounting`: the task records would otherwise
duplicate the job and let a slave host's values stand in for it (they are skipped,
since a PE task is a step and sacct emits no step rows), and qacct can briefly
return only the task records before the master lands -- a window in which the shim
reports no row at all, which reads as "unknown, keep polling" and self-corrects on
the next poll.

## Outcome mapping, before and after

Only the clean-exit code was ever lost. Everything derived from GE's `failed`
field was intact under a PE on every version, which is what kept `sacct` usable as
a completion signal even on the affected releases:

| Outcome | `failed` | shim State | shim ExitCode | correct on <= 9.1.4? |
|---------|----------|------------|---------------|----------------------|
| exited 0 | 0 | `COMPLETED` | `0:0` | yes |
| exited non-zero | 0 | `FAILED` | `<code>:0` | **no** -- reported `COMPLETED` / `0:0` |
| killed / `scancel` / signaled | 100 | `CANCELLED` | `0:9` | yes |
| qmaster `h_rt` limit | 37 | `TIMEOUT` | `0:9` | yes |
| node or execd lost the job | 22 | `NODE_FAIL` | - | yes |
| prolog / PE / epilog failure | other | `FAILED` | `<failed>:0` | yes |

So a job that **crashed** was always reported correctly; only a job that **exited
with a non-zero status of its own accord** was misreported as successful, and only
on 9.1.4 and earlier.

### Which integration this actually touched

Of the [recipes](../../recipes/), **submitit is the only one that reads job state
from `sacct`** -- and Hydra with it, since `hydra/launcher: submitit_slurm` drives
submitit. `clearml` polls `squeue` (live state), not the accounting file. Every
other recipe (lightning, jax, flax, ray, accelerate, deepspeed) bootstraps from the
fabricated `SLURM_*` environment and never asks how a job ended.

submitit survived the bug because it decides success from a **result pickle** on
the shared filesystem, not from the exit code; `sacct` is only its backstop for a
job that dies without writing one, and a terminal state -- even a wrong one -- was
enough to stop the polling. So no submitit user ever hung or silently lost a
failure.

What was wrong is the `sacct` row itself, and 9.1.5 fixes it. Re-run of
`docs/recipes/submitit/submitit_smoke.py` on 9.1.5 (submitit 1.5.4): identical
verdict, truthful row.

```
[ok] single submit -> 5
[ok] map_array -> [11, 22, 33, 44]
[ok] failure surfaced: FailedJobError
[result] SUBMITIT OK

$ sacct -o JobID,State,ExitCode,NodeList --parsable2 -j 20   # the boom() job
20|FAILED|1:0|ocs-master
```

On 9.1.4 that last row read `COMPLETED` / `0:0` -- so scanning a sweep's states for
failures was useless and you had to open every pickle. It is now a usable signal.
Hydra's `--multirun lr=0.1,0.01,0.001` was re-run too and is unchanged (3 tasks,
one per node, same losses).

## Verified on 9.1.5

OCS 9.1.5 (250826-0734), 3-node cluster from [`test/cluster`](../../../test/cluster),
2026-08-28:

```
$ sbatch --wrap='exit 3'          -> qacct: failed 0  exit_status 3
$ sacct -j <id> -o JobID,State,ExitCode --parsable2
JobID|State|ExitCode
17|FAILED|3:0
```

A 3-node, 3-task job whose script ends in `exit 3` reports the same. The full e2e
suite is green (122 assertions across 11 checks), and `test/e2e/91_sacct.sh` now
asserts this path directly -- gated on the version the cluster is actually
running, so the same suite still passes against 9.0.10, where it skips with
`exit status under control_slaves TRUE is lost on OCS 9.0.10 (fixed in 9.1.5)`.

**No shim change was required.** `acctState` and `acctExitCode` already mapped
`failed 0 / exit_status 3` to `FAILED` / `3:0`; they were exercised by unit tests
against synthetic records the whole time, which is why the fix was live the moment
OCS started recording the status.

One incidental improvement: 9.1.5 also records the signal in `exit_status` for a
job that dies by one (`kill -TERM $$` gives `failed 100 / exit_status 143`, where
9.0.10 gives `failed 100 / exit_status 0`). The shim still reports `0:9` for any
`failed 100`, which is why that path worked on the old releases; reporting the
real signal number where it is available would be a small refinement, not a fix.

## Still relevant on older clusters

On OCS 9.1.4 and earlier, a job whose failure must be visible in `sacct` has to
die by signal or write its own status, e.g. end the script with:

```bash
set -e            # or: cmd || kill -TERM $$
```

`set -e` alone does not help (the shell still exits with a status). Aborting via a
signal does, because that path sets `failed=100`.

The real fix is to upgrade: OCS 9.1.5 or newer.

## See also

- `internal/gedata/accounting.go` - `acctState` / `acctExitCode`
- `test/e2e/91_sacct.sh` - the e2e assertions, including the version gate
- `internal/cli/sacct/sacct.go` - the package-level note on this
