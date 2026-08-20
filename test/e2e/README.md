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
| `10_env` | the PE hook fabricates `SLURM_NNODES/NTASKS/JOB_NODELIST/JOB_ID/MASTER_ADDR` |
| `20_srun` | `srun -n` fans ranks across nodes, per-rank identity, `-l` labels |
| `30_sbatch` | `#SBATCH` -> `qsub -terse` -> `Submitted batch job <id>`, job runs |
| `40_squeue_scancel` | `squeue` lists a live job; `scancel` removes it |
| `50_scontrol` | `scontrol show hostnames` expands a compressed nodelist, one per line |
| `60_gpu` | a fake RSMAP grant -> `SLURM_JOB_GPUS` + per-rank `CUDA_VISIBLE_DEVICES` |
| `70_reject` | an impossible `srun` is rejected before launch (exit 1, no hang) |
| `80_sinfo` | `sinfo` shows live node counts/states from GE (not `n/a` placeholders) |
| `90_array` | `--array`: 0-based SLURM indices over 1-based GE tasks in the job env, `srun`, `sacct` and `scancel`; a `%a` batch path GE cannot express is substituted, not dropped (the Hydra/submitit shape) |
| `91_sacct` | `sacct` reporting: the default column set, every `--format` field, `ExitCode` as `code:signal`, `-P` vs `--parsable2`, `-X`, `-u`/`-s`/`-S`/`-E` selection (window applied to live rows too), aliases, and the exact `JobID|State|NodeList` shape submitit polls |

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

## Fixtures (`fixtures/<ocs-version>/`)

`capture.sh` records the version-sensitive GE outputs the shim parses -- the
granted RSMAP (`qstat -xml -j`), queue/host states (`qstat -xml -f`), complexes
and the `make` PE (`qconf -sc/-sp/-se`). Diff them across versions; when a format
shifts, fix the parser in `internal/gedata/` and commit the new version's fixture
as a regression baseline.
