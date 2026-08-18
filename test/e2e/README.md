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

Each check is a self-contained process that exits non-zero on failure; `run.sh`
fails if any did.

## Fixtures (`fixtures/<ocs-version>/`)

`capture.sh` records the version-sensitive GE outputs the shim parses -- the
granted RSMAP (`qstat -xml -j`), queue/host states (`qstat -xml -f`), complexes
and the `make` PE (`qconf -sc/-sp/-se`). Diff them across versions; when a format
shifts, fix the parser in `internal/gedata/` and commit the new version's fixture
as a regression baseline.
