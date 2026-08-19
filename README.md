# slurm-shim

**Drop-in SLURM command compatibility for [Open Cluster Scheduler](https://github.com/hpc-gridware/clusterscheduler) — run unmodified AI workloads.**

`sbatch`, `srun`, `squeue`, `scancel` and friends, translated to Open Cluster Scheduler (OCS) submissions — including parallel environment (PE) allocations for multi-node training. The goal: existing launchers and SLURM batch scripts keep working, exporting the `SLURM_*` environment the frameworks expect, without a scheduler migration.

> **Status: pre-release** The six client commands and the `SLURM_*` environment contract are implemented and covered by a green unit-test suite, and the launch mechanics have been grounded against real Open Cluster Scheduler 9.0.10 output (RSMAP, `qstat`, `qconf`, `qrsh` signatures). What has **not** happened yet is an end-to-end run on a live multi-GPU cluster. Treat every "supported" below as "implemented + unit-tested", not "battle-tested". If a flag isn't listed as supported, assume it doesn't work and [open an issue](../../issues).

https://github.com/user-attachments/assets/fa13c0c7-1e13-4fa3-b7fa-5ce421ba9160

---

## Why

Among open-source schedulers, SLURM has become the default integration target for many tooling —  tutorials, `submitit`, Hugging Face `accelerate`, and `dask-jobqueue` assume it out of the box. That's a fact about tool defaults, not about scheduling: production clusters run on a range of schedulers, and the Grid Engine lineage in particular still drives fleets in EDA, life sciences, and engineering — under active development today as Open Cluster Scheduler, with first-class GPU handling via RSMAP.

This shim bridges the two worlds: keep your SLURM-native tooling, run it on OCS. It is a single static Go binary (busybox-style symlink dispatch) that shells out to the GE clients (`qrsh`, `qstat`, `qsub`, `qdel`, `qconf`, `qmod`) and fabricates the SLURM environment inside GE PE jobs.

## Quickstart

> **TODO:** there is no installer yet. Today you build the binary and lay down the symlink farm by hand. `install.sh` and packaged releases are planned.

```bash
git clone https://github.com/hpc-gridware/slurm-shim
cd slurm-shim
make build                       # -> bin/slurm-shim (static, CGO-off)
# The one binary answers to every command via argv[0]; create the symlinks:
for cmd in sbatch srun squeue scancel scontrol sinfo; do ln -sf slurm-shim bin/$cmd; done
export PATH="$PWD/bin:$PATH"

# Point it at a config that maps your partitions to GE queues + PEs:
export SLURM_SHIM_CONFIG=/etc/slurm-shim/config.yaml   # see Configuration below

sbatch train.sh
squeue
```

`train.sh` — a SLURM-style script. Note the honest caveats inline:

```bash
#!/bin/bash
#SBATCH --job-name=llm-train
#SBATCH --partition=gpu           # mapped to a GE queue + PE + slot count
#SBATCH --nodes=4                 # feeds the slot count; GE's PE places the nodes
#SBATCH --ntasks-per-node=1
#SBATCH --cpus-per-task=8
# #SBATCH --gpus-per-node / --mem / --time / --array / --dependency ARE translated
# (see the matrix); GPUs need a GPU-configured partition/PE with an RSMAP complex.

srun torchrun --nnodes=4 --nproc-per-node=8 train.py
```

`sbatch` maps this to `qsub -terse -q <queue> -pe <pe> <slots> ...` (partition, job name, output/error, workdir), prints `Submitted batch job <id>`, and at runtime the PE job exports the `SLURM_*` variables your application reads.

*(TODO: record an asciinema/GIF of sbatch -> squeue -> job output.)*

## Try it without a cluster

One command stands up a real 3-node Open Cluster Scheduler cluster (in Docker) with slurm-shim installed, then runs a job that shows `srun` fanning ranks across nodes. Only Docker + a Go toolchain are needed:

```bash
make cluster-up          # clone quickinstall, boot OCS 9.1.4, install the shim
make demo                # multi-node srun fan-out (per-rank SLURM_PROCID/nodelist)
make demo-gpu            # per-rank CUDA_VISIBLE_DEVICES from a fake RSMAP grant
make cluster-down        # stop (add ARGS=-v to also wipe the OCS install)
```

Pick the OCS version with `OCS_VERSION=9.0.10 make cluster-up`. The cluster comes from the sibling [`quickinstall`](https://github.com/hpc-gridware/quickinstall) repo **unmodified** (cloned on demand, not vendored). See [`test/cluster/`](test/cluster/) for details and [`test/e2e/`](test/e2e/) for the `make e2e` end-to-end + version-compatibility suite.

## What is implemented (unit-tested, partially cluster validated)

Grounded in the [compatibility matrix](#compatibility-matrix) below:

- Multi-node PyTorch **DDP/FSDP** via `sbatch` + `srun torchrun` — the core path.
- Hugging Face **`accelerate launch`** and **torchrun** multi-node (env + `scontrol show hostnames`).
- **DeepSpeed** and **Ray** (and multi-node vLLM) via the [`docs/recipes/`](docs/recipes/) launch patterns.
- The full per-rank `SLURM_*` environment, including compressed nodelists and per-rank `CUDA_VISIBLE_DEVICES` from GE RSMAP grants.

- **submitit** (submit Python functions and arrays) via [`docs/recipes/submitit/`](docs/recipes/submitit/) — `sacct`, `sbatch --array`, and 0-based array tracking are implemented and verified live on the OCS test cluster.
- **JAX** (multi-process, multi-node) via [`docs/recipes/jax/`](docs/recipes/jax/) — `jax.distributed.initialize()` auto-detects the shim's environment with no glue and no PMI; verified live forming a 3-node process group on the OCS test cluster.

Partial / not yet: `dask-jobqueue`, Nextflow/Snakemake executors — see the matrix.

> **TODO:** add runnable `examples/` and a community `tests/` harness so the matrix below can be verified on a real cluster and pass/fail reports filed as issues. Neither exists yet.

## Compatibility matrix

This section is the contract. `✅` implemented (unit-tested) / `⚠️` partial (see note) / `❌` not implemented / `🚧` planned.

### Commands

| Command | Status | Notes |
|---|---|---|
| `sbatch` | ✅ | `#SBATCH` directives -> `qsub -terse`; prints `Submitted batch job <id>`. Flag coverage is limited (below). |
| `srun` (inside allocation) | ✅ | One process per task over `qrsh -inherit` tight integration; per-rank env + `CUDA_VISIBLE_DEVICES`. See [srun notes](#srun-semantics). |
| `srun` (standalone) | ⚠️ | `standalone: local` runs the command with a synthetic single-node env; default `standalone: reject` exits 1. |
| `squeue` | ✅ | Backed by `qstat -xml`. Default 8-column format + `-o/--format`, `-j`, `-u`, `-h`. No `--json`. |
| `scancel` | ✅ | Cancel maps to `qdel` (array `scancel N_k` -> `qdel N -t k+1`, 0-based; `-u` passthrough). `scancel --signal` (submitit's checkpoint-preempt) maps to `qmod -rj` (reschedule -> delivers SIGUSR2 to a `-notify` job and restarts it). |
| `scontrol show hostnames` / `show job` / `requeue` | ✅ | `show hostnames` nodelist expansion; `show job <id>` renders a minimal record (from the in-job layout, else looked up in GE via `qstat`); `requeue` -> `qmod -rj` (task-scoped for `<id>_<task>`). |
| `sinfo` | ⚠️ | Bare `sinfo` prints the partition table with live node counts, states (idle/mix/allocated/drain/down), and a compressed nodelist from `qstat -f`; `-V`. Flags are ignored; degrades to a config-only listing when GE is unreachable. |
| `sacct` | ✅ | Minimal, submitit-oriented: `-o JobID,State,NodeList --parsable2 -j <id>` (repeatable/comma ids). State from `qstat` (live) + `qacct` (finished, via go-clusterscheduler); 0-based array elements; unknown ids omitted. Not a full `sacct`. |
| `salloc` | ❌ | Not implemented. |

### `sbatch` flags

`sbatch` translates a **partition into a queue + PE + slot count**, passes through name/output/error/workdir, and maps the array and resource flags (`--array`, `--time`, `--mem`, `--gpus*`, `--dependency`, `--signal`) to their GE equivalents. The geometry flags feed the slot computation.

| Flag | Status | Mapped to |
|---|---|---|
| `--partition` / `-p` | ✅ | queue + PE + slots, via `partitions` config; falls back to `default_partition` when omitted |
| `--nodes` / `-N` | ✅ | feeds the slot count (GE's PE `allocation_rule` places the nodes) |
| `--ntasks` / `-n` | ✅ | feeds the slot count |
| `--ntasks-per-node` | ✅ | feeds the slot count |
| `--cpus-per-task` / `-c` | ✅ | feeds the slot count (`per-task` rule) |
| `--job-name` / `-J` | ✅ | `qsub -N` |
| `--output` / `--error` | ✅ | passed to `qsub -o`/`-e`; SLURM `%j`/`%A`/`%a`/`%x`/`%u`/`%N` filename patterns are translated to GE `$JOB_ID`/`$TASK_ID`/`$JOB_NAME`/`$USER`/`$HOSTNAME`; per-task streams from `srun` also expand `%A`/`%a` (0-based). SLURM defaults hold: no `--output` -> `slurm-<jobid>.out` in the submit dir (non-array), no `--error` -> stderr merged into stdout (`qsub -j y`) |
| `--chdir` / `-D` | ✅ | `qsub -wd`; without it the job runs in the **submit directory** (`qsub -cwd`), matching SLURM's default (GE's own default would be `$HOME`) |
| `--wrap` | ✅ | wraps the command string in a submitted script |
| `--gpus` / `--gpus-per-node` / `--gres=gpu:` | ✅ | `qsub -l <gpu.gres_complex>=<n>` (needs a GPU-configured partition/PE with an RSMAP complex) |
| `--gpus-per-task` | ✅ | scaled to a per-node request (`gpus-per-task x ntasks-per-node`) for the same `-l` path, and binds each task to its own devices (as SLURM's implied `--gpu-bind=per_task`) |
| `--gpu-bind` | ✅ | `none` (SLURM default: the node's whole grant stays visible to every task) or `per_task[:n]`; also honored as an `#SBATCH` directive via `SLURM_GPU_BIND` |
| `--mem` / `--mem-per-cpu` | ✅ | `qsub -l <memory_complex>=<n>` (default `h_vmem`; `4GB`->`4G`). Note `h_vmem` is virtual-address-space enforced — set `memory_complex` to `mem_free`/`h_rss` on GPU clusters |
| `--time` / `-t` | ✅ | `qsub -l h_rt=<sec>` (with `-l s_rt` grace when `--signal` gives a lead time) |
| `--array` | ✅ | `qsub -t 1-<n>` + `-tc` (from `%p`); SLURM 0-based indices are preserved end-to-end (env, filenames, `sacct`) |
| `--dependency` | ✅ | `after*`/`afterany`/`afterok` -> `-hold_jid` (GE releases on completion; `afterok` success-gating is approximated) |
| `--signal` | ✅ | `qsub -notify -r y` (GE sends SIGUSR2 before a kill/reschedule -- submitit's preempt signal -- and the job is rerunnable), plus `-l s_rt=h_rt-lead` as an early SIGUSR1 warning. With `scancel --signal`/`scontrol requeue` -> `qmod -rj`, this makes submitit checkpoint-then-requeue work |
| `--export` | ✅ | SLURM default `ALL` -> `qsub -V` (full submit env forwarded, so `PATH`/`PYTHON_BIN` reach the job like on SLURM); `NONE` -> nothing; a `VAR=val` list -> `qsub -v` per entry; `ALL,VAR=val` composes. Newline-valued vars are flattened to spaces by GE |
| `--exclusive` | ❌ | not translated (warn-and-ignored) |

Unknown/unsupported `#SBATCH` directives (including all Pyxis `--container-*`) are **warn-and-ignored**, not errors — deliberately, so clearml-agent's rendered templates submit. 🚧 A strict mode that fails loud on genuinely dropped directives is planned.

### `srun` flags

| Flag | Status |
|---|---|
| `-N/--nodes`, `-n/--ntasks`, `--ntasks-per-node`, `-c/--cpus-per-task` | ✅ |
| `-w/--nodelist` (subset of the allocation) | ✅ |
| `--gpus-per-task` | ✅ (per-rank contiguous GPU partition; over-subscription errors) |
| `--export=ALL\|NONE\|<list>` | ✅ |
| `-o/--output`, `-e/--error` (`%j %J %t %n %N %s %%` patterns) | ✅ |
| `-l/--label`, `-K/--kill-on-bad-exit`, `-J/--job-name`, `-D/--chdir`, `--quiet`, `-v`, `-V` | ✅ |
| `--mpi=none` | ✅ (no-op) |
| `--mpi=<anything else>` (e.g. `pmix`) | ❌ (hard-errors by design — no PMI/PMIx; see below) |

### `SLURM_*` environment variables

This is the strongest area — the fabricated environment is the whole point, and it is complete and unit-tested.

| Variable | Status |
|---|---|
| `SLURM_JOB_ID` / `SLURM_JOBID` | ✅ |
| `SLURM_JOB_NODELIST` / `SLURM_NODELIST` | ✅ compressed hostlist (`node[001-003,007]`), PE_HOSTFILE first-seen order (not sorted) |
| `SLURM_NNODES` / `SLURM_JOB_NUM_NODES` | ✅ |
| `SLURM_NTASKS` / `SLURM_NPROCS` | ✅ |
| `SLURM_NTASKS_PER_NODE` / `SLURM_TASKS_PER_NODE` | ✅ (per-node counts; omitted when non-uniform — see limitations) |
| `SLURM_PROCID` / `SLURM_LOCALID` / `SLURM_NODEID` | ✅ (per-rank, via `srun`) |
| `SLURM_ARRAY_TASK_ID` / `_TASK_COUNT` / `_JOB_ID` (+ min/max/step) | ✅ (from GE `SGE_TASK_ID`) |
| `SLURM_CPUS_PER_TASK` / `SLURM_CPUS_ON_NODE` | ✅ |
| `SLURM_GPUS_ON_NODE` / `SLURM_JOB_GPUS` (+ per-rank `CUDA_VISIBLE_DEVICES`) | ✅ (from GE RSMAP grant; `gpu.isolation: cgroup` passes GE's masking through instead) |
| `SLURM_MEM_PER_NODE` | ✅ (from the job's requested memory complex) |
| `SLURM_SUBMIT_DIR` / `SLURM_SUBMIT_HOST` / `SLURM_JOB_PARTITION` | ✅ |
| `MASTER_ADDR` / `MASTER_PORT` | ⚠️ off by default (`export_master_addr: false`) — derive in your job script, or enable in config |

### srun semantics

- `srun` launches one process per task over **`qrsh -inherit` tight integration**: the master host runs locally, slave hosts via `qrsh`, so `sge_execd` owns accounting and cleanup (`qdel`/wallclock kill). The StepSpec (environment, rank list) and signals travel over a single **authenticated TCP control channel** dialed back from each stepper — not argv, not shared files.
- **MPI: no PMI/PMIx.** `srun --mpi=none` is a no-op; any other `--mpi=` value hard-errors. MPI jobs must use the PE's native `mpirun` tight integration, not `srun`. A script calling `deepspeed.init_distributed()`/mpi4py **without** rank vars set degrades to a single process — use the `torchrun` recipe, which sets them.
- **Not replicated:** full SLURM job-step semantics (`--overlap`, heterogeneous steps), `sattach`, `salloc`, and a full `sacct` (a minimal submitit-oriented `sacct` exists). Signal forwarding (SIGINT/TERM/HUP/USR1/USR2/QUIT) over the channel **is** implemented, as is kill-on-bad-exit.

### Known limitations

- No `salloc` / `sattach`; no job-step overlap or heterogeneous steps. `sacct` is minimal (submitit-oriented), not a full implementation.
- `sbatch` translates `--time`/`--mem`/`--array`/`--dependency`/`--gpus`/`--gres`/`--signal`; `--exclusive` is still warn-and-ignored. Non-contiguous `--array=1-5,20` (comma lists) are rejected — GE arrays are a single contiguous range.
- No PMI/PMIx (MPI via the PE's `mpirun`).
- **Not yet validated end-to-end on a live GPU cluster** — unit-tested and fixture-grounded only.
- PyTorch Lightning requires a **homogeneous** allocation (it raises if `SLURM_NTASKS_PER_NODE` is absent with `ntasks>1`); the fabricator warns on non-uniform per-node counts.

## Requirements

- **Open Cluster Scheduler** — launch mechanics grounded against OCS **9.0.10** (fixtures in the repo). The binary itself has not been deployed on a live cluster yet.
- Also targets **Gridware Cluster Scheduler** (same lineage); other SGE-compatible variants: untested.
- A **parallel environment** with `control_slaves TRUE` for multi-node jobs (the shim's preflight checks this). 🚧 A `docs/pe-setup.md` guide is planned; the PE hook scripts are in [`docs/install/`](docs/install/).
- Runtime deps: only the GE client tools (`qrsh`, `qstat`, `qsub`, `qdel`, `qconf`, `qmod`, `qacct`, `qhost`) and the config file. The binary is static (CGO off, `osusergo`/`netgo`).

## Configuration

The shim reads a YAML file at `$SLURM_SHIM_CONFIG`, else `/etc/slurm-shim/config.yaml`. Key settings:

```yaml
partitions:                       # SLURM --partition -> GE queue + PE + slots
  gpu:   {queue: gpu.q, pe: gpu.pe, slots: "per-task"}   # per-task = ntasks * cpus_per_task
  batch: {queue: all.q, pe: smp.pe, slots: "16"}          # or a fixed slot count
default_partition: batch          # sbatch without -p lands here (SLURM's DEFAULT)
pes:
  gpu.pe: {task_policy: gpu}      # gpu | node | slot -> how SLURM_NTASKS is derived
  smp.pe: {task_policy: slot}     # one task per slot (MPI-style)
gpu:
  discovery: qstat-gres           # RSMAP grant -> CUDA_VISIBLE_DEVICES
  isolation: shim                 # shim (per-rank masking) | cgroup (GE devices_allow)
  gres_complex: gpu
  bind: none                      # none (SLURM default: whole grant visible to every
                                  # task) | per-task (split the grant among tasks)
memory_complex: h_vmem            # source for SLURM_MEM_PER_NODE ("" disables)
export_master_addr: false         # set true to publish MASTER_ADDR/MASTER_PORT
launcher: qrsh-inherit            # qrsh-inherit | local (dev/test)
```

🚧 A full configuration reference is planned; the authoritative source today is [`internal/config`](internal/config/config.go).

## Support

The shim is open source (Apache-2.0) and community-supported via GitHub issues — bug reports with the failing SLURM script attached are the most useful.

*(TODO: confirm internally whether commercial [Gridware Cluster Scheduler](https://hpc-gridware.com/gridware-cluster-scheduler/) support will cover the shim before advertising it here.)*

## Contributing

🚧 `CONTRIBUTING.md` and `good-first-issue` labels are not set up yet. Most future "add support for sbatch flag X" work touches a single mapping table (`internal/cli/sbatch/translate.go`); the `SLURM_*` contract lives in `internal/fabricator`. Run `make test` and `make lint` before submitting.

## Trademarks

Developed by [HPC-Gridware](https://hpc-gridware.com), the company behind Open Cluster Scheduler. This project is not affiliated with or endorsed by SchedMD®. SLURM is a trademark of SchedMD LLC.

## License

Apache-2.0 — see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE). Copyright 2026 HPC-Gridware.
