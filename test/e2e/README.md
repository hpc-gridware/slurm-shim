# End-to-end + OCS version-compatibility suite

Black-box checks that drive the installed shim on a **running** cluster (stood up
by [`../cluster`](../cluster)) and assert the SLURM-facing behavior end to end,
plus a fixture capture for cross-version compatibility.

## Run it

```bash
make cluster-up          # first: a real cluster with the shim installed
make e2e                 # run every check against it
make capture-fixtures    # dump this OCS version's parser inputs to fixtures/<ver>/
make e2e-matrix          # for each OCS version: down -v; up; e2e; capture
```

## Checks (`NN_*.sh`, run in order by `run.sh`)

| Check | Asserts |
|-------|---------|
| `05_hook` | the sourcing hook ENFORCES its abort paths (a fabrication-failed sentinel, and `SLURM_SHIM_HOOK_MISSING_ENV=abort`, end the job even when sourced bare) while the default "continue" policy and a good environment let it run; and it is the TRUST BOUNDARY for the per-job state (todos/029, 030): a symlinked, group/world-writable, or foreign-owned sentinel/environment/state dir/`TMPDIR` is reported and ignored, and there is no `/tmp` fallback. Runs the installed hook against fabricated `TMPDIR`s in the master container, no scheduling needed |
| `06_starter` | an **unmodified** SLURM script (no hook line) gets `SLURM_*` through the queue's `starter_method`; `scontrol show hostnames` expands; `srun` fans out; a native `qsub` job in the same queue is unaffected; a failed fabrication fails the job (exit 1; the accounting half asserted on OCS >= 9.1.5 only) and no queue instance goes `E`; a job-level `MISSING_ENV=abort` does not kill the steppers; a shebang-less script runs; the install tree is root-owned. Regressions for the review findings: a co-tenant-planted world-writable `TMPDIR` with a sentinel neither kills a native job nor denies a PE job its environment (029: the fabricator reclaims the dir); under `posix_compliant` a `-S /bin/bash` job starts as a login shell and under `script_from_stdin` the script still runs (031: the starter honours `SGE_STARTER_SHELL_*`). Mutates `smp`'s `start_proc_args` and `all.q`'s `shell_start_mode`, both restored by trap |
| `07_interactive` | `srun --pty` outside an allocation becomes an interactive `qrsh` session: real pty, the `SLURM_*` environment, the invocation dir (`-cwd`), exit-status propagation, quoted argv intact, `--account` -> `SLURM_JOB_ACCOUNT`, a 2-node session with inner `srun` fan-out, dry run prints the qrsh line and creates no job, `QRSH_WRAPPER` scrubbed, and non-pty standalone `srun` still rejected. Uses `script(1)` for a pty |
| `10_env` | the PE hook fabricates `SLURM_NNODES/NTASKS/JOB_NODELIST/JOB_ID/MASTER_ADDR`; this is the one check whose job script keeps an explicit `. slurm-shim-source-hook.sh` line, so the no-starter fallback (and double-sourcing under a starter) stays covered |
| `20_srun` | `srun -n` fans ranks across nodes, per-rank identity, `-l` labels |
| `30_sbatch` | `#SBATCH` -> `qsub -terse` -> `Submitted batch job <id>`, job runs |
| `31_sbatch_resources` | the resource flags reach GE and take effect: `--time` -> `h_rt` (and GE enforces it), `--mem` -> the memory complex, `#SBATCH --gres=gpu:N` -> RSMAP grant -> `SLURM_JOB_GPUS` -> step `CUDA_VISIBLE_DEVICES`, `--dependency` -> `-hold_jid`, `--array %p` -> `-tc`, `--signal` -> `-notify -r y`; malformed values rejected at submit time |
| `40_squeue_scancel` | `squeue` lists a live job; `scancel` removes it |
| `50_scontrol` | `scontrol show hostnames` expands a compressed nodelist, one per line |
| `60_gpu` | a fake RSMAP grant -> `SLURM_JOB_GPUS` + per-rank `CUDA_VISIBLE_DEVICES` |
| `70_reject` | an impossible `srun` is rejected before launch (exit 1, no hang) |
| `80_sinfo` | `sinfo` shows live node counts/states from GE (not `n/a` placeholders) |
| `90_array` | `--array`: 0-based SLURM indices over 1-based GE tasks in the job env, `srun`, `sacct` and `scancel`; a `%a` batch path GE cannot express is substituted, not dropped (the Hydra/submitit shape) |
| `91_sacct` | `sacct` reporting: the default column set, every `--format` field, `ExitCode` as `code:signal`, `-P` vs `--parsable2`, `-X`, `-u`/`-s`/`-S`/`-E` selection (window applied to live rows too), aliases, the exact `JobID|State|NodeList` shape submitit polls, and (on OCS >= 9.1.5) that a job exiting non-zero reports `FAILED` / `3:0` |

Each check is a self-contained process that exits non-zero on failure; `run.sh`
fails if any did.

## Adding a check

**Rule: if we verified something works and wrote it down, it gets a check here.**
A recipe's "Validated on the OCS test cluster" section is a claim; without a check
it silently rots, and nothing catches the regression. Every claim in a recipe
should be reducible to an assertion in this suite.

**Keep checks cheap.** These run nightly on a GitHub-hosted runner (2 cores, ~14 GB
disk, 45-minute cap for the whole suite) that also has to boot a 3-node OCS
cluster, so a check must not:

- download large artifacts -- no PyTorch, JAX or CUDA wheels (multi-GB, and they
  blow the disk before they blow the timeout);
- depend on a Python environment the runner does not already have;
- sleep for minutes or submit long-running jobs.

Prefer **pure shell that asserts the shim's SLURM-facing behavior**. Almost every
framework claim reduces to one: "does the job get the right `SLURM_*` values, in
the right files, with the right ids?" is testable with `echo` and does not need
the framework installed. `90_array` is the model -- it pins the whole
Hydra/submitit array contract with no dependencies at all.

Framework-level runs (an actual JAX or Lightning job) stay **manual**, recorded in
the recipe with captured output; do not add them here.

Register a new check in the `checks=(...)` array in `run.sh` and add a row to the
table above.

**Never submit a job whose resource request cannot be satisfied.** It does not sit
harmlessly in `qw`: GE tries it on every host, fails, and marks `all.q` **QERROR
on all three nodes**, which blocks every check that runs after it -- the failure
then looks like a bug in someone else's check. The test cluster's exec hosts
define only `slots` and `gpu` in `complex_values`, so any `h_vmem` request is
unsatisfiable. `31_sbatch_resources.sh` works around this by parking such a job
behind a `--dependency` hold: held in `hqw` it is never dispatched, and
`qstat -j` still exposes `hard_resource_list`, which is all a request-shape
assertion needs. If a check does poison the queue, `qmod -c all.q` clears it.

**Assert request shape, not enforcement, for anything site-configurable.** The
memory complex is a config knob (`memory_complex`, which the README recommends
setting to `mem_free`/`h_rss` on GPU clusters), so asserting an OOM kill would
fail on a correctly configured site.

## Fixtures (`fixtures/<ocs-version>/`)

`capture.sh` records the version-sensitive GE outputs the shim parses -- the
granted RSMAP (`qstat -xml -j`), queue/host states (`qstat -xml -f`), complexes
and the `make` PE (`qconf -sc/-sp/-se`). Diff them across versions; when a format
shifts, fix the parser in `internal/gedata/` and commit the new version's fixture
as a regression baseline. Most of the diff between two captures is noise
(hostnames, load averages, job ids, timestamps) -- read past it.

### What changed 9.1.4 -> 9.1.5

Captured 2026-08-28 against OCS 9.1.5 (250826-0734):

- **New builtin complex `devices` (`devs`, RESTRING, requestable, not
  consumable)** in `qconf -sc`. No effect on the shim: GPU discovery matches the
  RSMAP complex by its configured name (`gpu.gres_complex`, default `gpu`), so a
  new complex cannot be mistaken for it, and `60_gpu` is green on 9.1.5.
- **`exit_status` is now recorded under a `control_slaves TRUE` PE.** A value
  change, not a format change -- the parser needed nothing. See
  [`../../docs/solutions/integration-issues/pe-jobs-lose-exit-status-in-accounting.md`](../../docs/solutions/integration-issues/pe-jobs-lose-exit-status-in-accounting.md).
- Everything else the shim parses (`qstat -f`, `qstat -xml -f`, `qstat -xml -j`
  granted RSMAP, `qconf -sp make`) is byte-identical in shape.
