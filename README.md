# slurm-shim

**Drop-in SLURM command compatibility for [Open Cluster Scheduler](https://github.com/hpc-gridware/clusterscheduler) — run AI tools that only speak SLURM, unmodified.**

`sbatch`, `srun`, `squeue`, `scancel` and friends, translated to Open Cluster Scheduler (OCS) submissions — including parallel environment (PE) allocations for multi-node training. The goal: existing launchers and SLURM batch scripts keep working, exporting the `SLURM_*` environment the frameworks expect, without a scheduler migration. That holds once a site has wired two Open Cluster Scheduler attributes (a PE `start_proc_args` and a queue `starter_method`, see [Quickstart](#quickstart)); nothing in the job script changes.

**Not the target: MPI.** OpenMPI, Intel MPI and MVAPICH already run natively on OCS/GCS through [Open Cluster Scheduler's own MPI integrations](https://github.com/hpc-gridware/clusterscheduler/tree/master/source/dist/mpi) — that path is better than anything a shim can offer, so use it (`srun --mpi=pmix` hard-errors by design). The same rule applies generally: **if your tool has a native Grid Engine integration, prefer it.** The shim is for tools that only speak SLURM — JAX, submitit, and the rest of the AI stack.

> **Status: pre-release** The seven client commands and the `SLURM_*` environment contract are implemented, unit-tested, and exercised by an end-to-end suite against live Open Cluster Scheduler clusters (9.0.10 and 9.1.5). GPU paths are validated through a fake RSMAP complex, **not** on real hardware. If a flag isn't listed as supported, assume it doesn't work and [open an issue](../../issues).

https://github.com/user-attachments/assets/fa13c0c7-1e13-4fa3-b7fa-5ce421ba9160

---

## Why

Among open-source schedulers, SLURM has become the default integration target for many tooling — tutorials, `submitit`, `jax.distributed`, and Hugging Face `accelerate` assume it out of the box. That's a fact about tool defaults, not about scheduling: production clusters run on a range of schedulers, and the Grid Engine lineage in particular still drives fleets in EDA, life sciences, and engineering — under active development today as Open Cluster Scheduler, with first-class GPU handling via RSMAP.

This shim bridges the two worlds: keep your SLURM-native tooling, run it on OCS. It is a single static Go binary (busybox-style symlink dispatch) that shells out to the GE clients (`qrsh`, `qstat`, `qsub`, `qdel`, `qconf`, `qmod`) and fabricates the SLURM environment inside GE PE jobs.

## Quickstart

> **TODO:** there is no installer or package yet. The steps below are done by hand; [`test/cluster/install-shim.sh`](test/cluster/install-shim.sh) performs exactly this sequence against the Docker test cluster and is the scripted reference until one exists.

Two parts: a one-time site install by an Open Cluster Scheduler (fka Sun Grid Engine) manager, then users submit.

```bash
# ---- one-time site install (an Open Cluster Scheduler manager does this) ----
git clone https://github.com/hpc-gridware/slurm-shim
cd slurm-shim
make build install-links   # bin/slurm-shim + the command symlinks: srun sbatch sacct
                           # squeue scancel scontrol sinfo slurm-shim-env slurm-shim-stepper
                           # (relative links, so bin/ copies as a unit)

# 1. Files. The SAME absolute path on EVERY node -- the qrsh envelope carries it as
#    the remote argv0. Root-owned and writable by nobody else: the starter runs as
#    the job user for every job in the queue, so whoever can edit it runs code in
#    every other user's jobs.
sudo install -d -o root -g root -m 755 /opt/slurm-shim/bin /opt/slurm-shim/etc /etc/slurm-shim
sudo cp -a bin/. /opt/slurm-shim/bin/                                                   # binary + symlinks
sudo install -o root -g root -m 755 docs/install/slurm-shim-starter.sh     /opt/slurm-shim/bin/slurm-shim-starter
sudo install -o root -g root -m 644 docs/install/slurm-shim-source-hook.sh /opt/slurm-shim/etc/slurm-shim-source-hook.sh
sudo tee /etc/slurm-shim/config.yaml >/dev/null <<'EOF'      # minimal; full key set under Configuration
default_partition: batch
partitions:
  batch: {queue: all.q, pe: make, slots: "per-task"}
pes:
  make: {task_policy: slot}
EOF
sudo chmod 644 /etc/slurm-shim/config.yaml
sudo chown -R root:root /opt/slurm-shim && sudo chmod -R go-w /opt/slurm-shim

# 2. Open Cluster Scheduler, once per PE and queue that config.yaml's partitions map to:
qconf -mattr pe    start_proc_args /opt/slurm-shim/bin/slurm-shim-env     <pe>     # fabricates SLURM_* per job
qconf -mattr queue starter_method  /opt/slurm-shim/bin/slurm-shim-starter <queue>  # injects it into every job

# 3. Verify, as a normal user, with no hook line anywhere:
export PATH=/opt/slurm-shim/bin:$PATH
sbatch -p <partition> -N 2 --wrap 'scontrol show hostnames; env | grep -c ^SLURM_'
cat slurm-<jobid>.out      # one hostname per line, then a count well above 0.
                           # Empty? The starter is not wired on that queue: qconf -sq <queue> | grep starter_method

# ---- users ----
export PATH=/opt/slurm-shim/bin:$PATH
sbatch train.sh
squeue
```

`train.sh` — a stock SLURM script with nothing shim-specific in it. Note the honest caveats inline:

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

`sbatch` maps this to `qsub -terse -q <queue> -pe <pe> <slots> ...` (partition, job name, output/error, workdir), prints `Submitted batch job <id>`, and at runtime the PE hook fabricates the `SLURM_*` variables and the queue's `starter_method` sources them before the script's first line. **The script needs no edit only because the site wired `starter_method`.** On a site that has not, a job script must source `/opt/slurm-shim/etc/slurm-shim-source-hook.sh` itself, or the site enables `wrapper_mode` (see [Configuration](#configuration)); the recipes under [`docs/recipes/`](docs/recipes/) carry that line so they run either way.

*(TODO: record an asciinema/GIF of sbatch -> squeue -> job output.)*

## Try it without a cluster

One command stands up a real 3-node Open Cluster Scheduler cluster (in Docker) with slurm-shim installed, then runs a job that shows `srun` fanning ranks across nodes. Only Docker + a Go toolchain are needed:

```bash
make cluster-up          # clone quickinstall, boot OCS 9.1.5, install the shim
make demo                # multi-node srun fan-out (per-rank SLURM_PROCID/nodelist)
make demo-gpu            # per-rank CUDA_VISIBLE_DEVICES from a fake RSMAP grant
make cluster-down        # stop (add ARGS=-v to also wipe the OCS install)
```

Pick the OCS version with `OCS_VERSION=9.0.10 make cluster-up`. The cluster comes from the sibling [`quickinstall`](https://github.com/hpc-gridware/quickinstall) repo **unmodified** (cloned on demand, not vendored). See [`test/cluster/`](test/cluster/) for details and [`test/e2e/`](test/e2e/) for the `make e2e` end-to-end + version-compatibility suite.

## What is implemented (unit-tested, validated on a live OCS cluster)

Grounded in the [compatibility matrix](#compatibility-matrix) below:

- Multi-node PyTorch **DDP/FSDP** via `sbatch` + `srun torchrun` — the core path.
- Hugging Face **`accelerate launch`** and **torchrun** multi-node (env + `scontrol show hostnames`).
- **DeepSpeed** and **Ray** (and multi-node vLLM) via the [`docs/recipes/`](docs/recipes/) launch patterns.
- The full per-rank `SLURM_*` environment, including compressed nodelists and per-rank `CUDA_VISIBLE_DEVICES` from GE RSMAP grants.

- **submitit** (submit Python functions and arrays) via [`docs/recipes/submitit/`](docs/recipes/submitit/) — `sacct`, `sbatch --array`, and 0-based array tracking are implemented and verified live on the OCS test cluster.
- **Hydra** (`--multirun` parameter sweeps) via [`docs/recipes/hydra/`](docs/recipes/hydra/) — `hydra/launcher: submitit_slurm` turns each sweep config into a cluster job; verified live fanning a sweep across 3 nodes.
- **JAX** (multi-process, multi-node) via [`docs/recipes/jax/`](docs/recipes/jax/) — `jax.distributed.initialize()` auto-detects the shim's environment with no glue and no PMI; verified live forming a 3-node process group on the OCS test cluster.
- **Flax** (data-parallel training, multi-node) via [`docs/recipes/flax/`](docs/recipes/flax/) — the same auto-detect, then a real optimizer over one global `jax.sharding` mesh spanning every process's devices, so a wrong environment fails loudly instead of training alone; verified live forming a 6-device mesh across the 3-node OCS test cluster with gradients reduced across hosts every step.

### Use the native Grid Engine path instead

These already speak Grid Engine — they need no shim, and the native route is the better one:

| Tool | Native Grid Engine support |
|---|---|
| **Dask** | [`dask_jobqueue.SGECluster`](https://jobqueue.dask.org/en/latest/generated/dask_jobqueue.SGECluster.html) — works with any Grid Engine derivative |
| **Nextflow** | the [`sge` executor](https://docs.seqera.io/nextflow/executor) — `process.executor = 'sge'` |
| **Snakemake** | the [`cluster-generic` executor plugin](https://snakemake.github.io/snakemake-plugin-catalog/plugins/executor/cluster-generic.html) (`--executor cluster-generic --cluster "qsub -terse"`), or the [SGE profile](https://github.com/Snakemake-Profiles/sge) |
| **JupyterHub** | [`batchspawner`](https://github.com/jupyterhub/batchspawner)'s [`GridengineSpawner`](https://github.com/jupyterhub/batchspawner/blob/main/SPAWNERS.md) |
| **MPI** (OpenMPI, Intel MPI, MVAPICH) | Open Cluster Scheduler tight integration via the PE's `mpirun` — see the shipped [MPI integrations](https://github.com/hpc-gridware/clusterscheduler/tree/master/source/dist/mpi) (PE templates and start/stop scripts) |

> **TODO:** add runnable `examples/` and a community `tests/` harness so the matrix below can be verified on a real cluster and pass/fail reports filed as issues. Neither exists yet.

## Compatibility matrix

This section is the contract. `✅` implemented (unit-tested) / `⚠️` partial (see note) / `❌` not implemented / `🚧` planned.

### Commands

| Command | Status | Notes |
|---|---|---|
| `sbatch` | ✅ | `#SBATCH` directives -> `qsub -terse`; prints `Submitted batch job <id>`. Flag coverage is limited (below). `--test-only` / `SLURM_SHIM_DRY_RUN` report without submitting ([dry run](#dry-run)). |
| `srun` (inside allocation) | ✅ | One process per task over `qrsh -inherit` tight integration; per-rank env + `CUDA_VISIBLE_DEVICES`. See [srun notes](#srun-semantics). Honors [dry run](#dry-run). |
| `srun --pty` (interactive) | ✅ | Outside an allocation, `srun --pty [flags] <cmd>` becomes an interactive `qrsh -now no -pty y` session on a compute node, with the full `SLURM_*` environment; `srun` inside it launches steps. See [Interactive sessions](#interactive-sessions). |
| `srun` (standalone, no `--pty`) | ❌ | A non-interactive `srun` outside an allocation exits 1 (`standalone: reject`). Run it inside an interactive session, or via `sbatch`. |
| `squeue` | ✅ | Backed by `qstat -xml`. Default 8-column format + `-o/--format`, `-j`, `-u`, `-h`. No `--json`. |
| `scancel` | ✅ | Cancel maps to `qdel` (array `scancel N_k` -> `qdel N -t k+1`, 0-based; `-u` passthrough). `scancel --signal` (submitit's checkpoint-preempt) maps to `qmod -rj` (reschedule -> delivers SIGUSR2 to a `-notify` job and restarts it). Honors [dry run](#dry-run). |
| `scontrol show hostnames` / `show job` / `requeue` | ✅ | `show hostnames` nodelist expansion; `show job <id>` renders a minimal record (from the in-job layout, else looked up in GE via `qstat`); `requeue` -> `qmod -rj` (task-scoped for `<id>_<task>`); honors [dry run](#dry-run). |
| `sinfo` | ⚠️ | Bare `sinfo` prints the partition table with live node counts, states (idle/mix/allocated/drain/down), and a compressed nodelist from `qstat -f`; `-V`. Flags are ignored; degrades to a config-only listing when GE is unreachable. |
| `sacct` | ✅ | Selection by `-j` (repeatable/comma ids), `-u` (comma list), `-s/--state`, `-S`/`-E`; `-o/--format` over JobID, JobName, State, ExitCode, Elapsed, Start, End, Submit, User, Partition, Account, AllocCPUS, NodeList, MaxRSS, TotalCPU (+ aliases like `JobIDRaw`, `NCPUS`); SLURM's default columns when `-o` is absent; `-P`/`--parsable2`/`-n`/`-X`. Data from `qstat -xml` (live) + `qacct` (finished, via go-clusterscheduler). No accounting DB behind it: no job-step rows, associations/QOS, or `--json`. Read [sacct fidelity](#sacct-fidelity) before relying on it. |
| `salloc` | ❌ | Not implemented. |

### `sbatch` flags

`sbatch` translates a **partition into a queue + PE + slot count**, passes through name/output/error/workdir, and maps the array and resource flags (`--array`, `--time`, `--mem`, `--gpus*`, `--dependency`, `--signal`) to their GE equivalents. The geometry flags feed the slot computation.

| Flag | Status | Mapped to |
|---|---|---|
| `--partition` / `-p` | ✅ | queue + PE + slots, via `partitions` config; falls back to `default_partition` when omitted |
| `--nodes` / `-N` | ✅ | feeds the slot count **and pins the node count**: on OCS 9.1.5+ the shim emits `qsub -par <slots-per-node>`, so the job gets exactly this many nodes. `-w e` rides along to refuse an unschedulable layout at submit, except when `--mem` is used with a load-sensor `memory_complex` (the `mem_free` default): `-w e` cannot see load values and would refuse a runnable job, so it is omitted and an unschedulable layout waits in the queue instead. Below 9.1.5 it only feeds the slot count and the PE's `allocation_rule` places the nodes (with a warning saying so) |
| `--ntasks` / `-n` | ✅ | feeds the slot count |
| `--ntasks-per-node` | ✅ | feeds the slot count **and pins tasks per node**, through the same `-par` path as `--nodes`. A layout Grid Engine cannot grant evenly (`-N 3 -n 7` -> 3,2,2) pins nothing and warns: a fixed allocation rule puts the same count on every node |
| `--cpus-per-task` / `-c` | ✅ | feeds the slot count (`per-task` rule). It scales the pinned rule too: `-N 3 --ntasks-per-node=2 -c 4` pins 8 **slots** per node, which is still 2 tasks |
| `--job-name` / `-J` | ✅ | `qsub -N` |
| `--output` / `--error` | ✅ | `qsub -o`/`-e`; SLURM `%j`/`%A`/`%a`/`%x`/`%u`/`%N` patterns are translated to GE `$JOB_ID`/`$TASK_ID`/`$JOB_NAME`/`$USER`/`$HOSTNAME` (a zero-pad width like `%3a` expands but is not padded). **Exception:** GE's `$TASK_ID` is a dense 1..N over the submitted range, so for a 0-based or stepped array a `%a` batch-level path is replaced by `<literal-dir>/slurm-$JOB_ID.$TASK_ID.{out,err}` with a warning; the per-task files `srun` writes keep the SLURM indices. SLURM defaults hold: no `--output` -> `slurm-<jobid>.out` (non-array), no `--error` -> merged into stdout (`qsub -j y`) |
| `--chdir` / `-D` | ✅ | `qsub -wd`; without it the job runs in the **submit directory** (`qsub -cwd`), matching SLURM's default (GE's own default would be `$HOME`) |
| `--wrap` | ✅ | wraps the command string in a submitted script |
| `--gpus` / `--gpus-per-node` / `--gres=gpu:` | ✅ | `qsub -l <gpu.gres_complex>=<n>` (needs a GPU-configured partition/PE with an RSMAP complex) |
| `--gpus-per-task` | ✅ | scaled to a per-node request (`gpus-per-task x ntasks-per-node`) for the same `-l` path, and binds each task to its own devices (as SLURM's implied `--gpu-bind=per_task`) |
| `--gpu-bind` | ✅ | `none` (SLURM default: the node's whole grant stays visible to every task) or `per_task[:n]`; also honored as an `#SBATCH` directive via `SLURM_GPU_BIND`. An explicit `none` overrides `--gpus-per-task` binding, as on SLURM |
| `--mem` / `--mem-per-cpu` | ⚠️ | `qsub -l <memory_complex>=<n>` (default `mem_free`; `4GB`->`4G`). **A scheduling filter, not a limit** — the job is placed where the memory is free, but is not killed for exceeding it. Do **not** set `memory_complex: h_vmem`: it caps virtual address space (`RLIMIT_AS`), and a CUDA context reserves tens of GB of it, so every GPU job dies at init. See [Memory requests](#memory-requests) |
| `--time` / `-t` | ✅ | `qsub -l h_rt=<sec>` (with `-l s_rt` grace when `--signal` gives a lead time) |
| `--array` | ✅ | `qsub -t 1-<n>` + `-tc` (from `%p`); SLURM 0-based indices are preserved end-to-end (env, filenames, `sacct`) |
| `--dependency` | ⚠️ | GE has one primitive, `-hold_jid`, which releases when every predecessor **finishes**. Only `afterany` means exactly that. `after` is *start*-gated in SLURM, so it becomes a wait-for-exit here (it will never release on a long-lived predecessor); `afterok`/`aftercorr` are approximated (they run anyway on failure, and `aftercorr` loses its per-element pairing); `afternotok` is inverted outright; `singleton` and the `+time` offset form yield no id, so **nothing is held**. Every one of those warns at submit time |
| `--signal` | ✅ | `qsub -notify -r y` (GE sends SIGUSR2 before a kill/reschedule -- submitit's preempt signal -- and the job is rerunnable), plus `-l s_rt=h_rt-lead` as an early SIGUSR1 warning. With `scancel --signal`/`scontrol requeue` -> `qmod -rj`, this makes submitit checkpoint-then-requeue work |
| `--export` | ✅ | SLURM default `ALL` -> `qsub -V` (full submit env forwarded, so `PATH`/`PYTHON_BIN` reach the job like on SLURM); `NONE` -> nothing; a `VAR=val` list -> `qsub -v` per entry; `ALL,VAR=val` composes. Newline-valued vars are flattened to spaces by GE |
| `--exclusive` | ❌ | not translated: it means *all of whatever the node has*, and sbatch does not query per-host capacity. Since 9.1.5 the shim does pin slots-per-node per job, so ask for the width explicitly (`--ntasks-per-node=<cores>`), or use a partition whose PE has `allocation_rule $pe_slots` sized to the node, or add an exclusive complex. Warn-and-ignored, with that advice in the warning |

Unknown/unsupported `#SBATCH` directives (including all Pyxis `--container-*`) are **warn-and-ignored**, not errors — deliberately, so clearml-agent's rendered templates submit. 🚧 A strict mode that fails loud on genuinely dropped directives is planned.

### `srun` flags

| Flag | Status |
|---|---|
| `-N/--nodes`, `-n/--ntasks`, `--ntasks-per-node`, `-c/--cpus-per-task` | ✅ |
| `-w/--nodelist` (subset of the allocation) | ✅ |
| `--gpus-per-task` | ✅ (per-rank contiguous GPU partition; over-subscription errors). Under `gpu.isolation: cgroup` GE masks per job, so per-task binding cannot be applied — `srun` warns instead of dropping it silently |
| `--export=ALL\|NONE\|<list>` | ✅ |
| `-o/--output`, `-e/--error` (`%j %J %t %n %N %s %%` patterns) | ✅ |
| `-l/--label`, `-K/--kill-on-bad-exit`, `-J/--job-name`, `-D/--chdir`, `--quiet`, `-v`, `-V` | ✅ |
| `--pty` (outside an allocation) | ✅ interactive session via `qrsh`; with `-p/--partition`, `-N/-n/--ntasks-per-node/-c`, `--time`, `--mem`, `--gres`/`--gpus`, `-A/--account`, `-J`. `--qos` warned (no GE analogue). See [Interactive sessions](#interactive-sessions) |
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
- **Not replicated:** full SLURM job-step semantics (`--overlap`, heterogeneous steps), `sattach` and `salloc`. Signal forwarding (SIGINT/TERM/HUP/USR1/USR2/QUIT) over the channel **is** implemented, as is kill-on-bad-exit.

### Interactive sessions

`srun --pty ... <command>` run **outside** an allocation becomes an interactive
Grid Engine session. The shim translates the SLURM flags and hands off to `qrsh`,
which owns the terminal, the wait for resources, signal handling and the exit
status:

```bash
# a shell on a compute node, 2 cores, 30-minute limit
srun --pty -p batch -c 2 --time 0:30:00 bash

# GPU debug shell (needs a GPU-configured partition)
srun --pty -p gpu --gres=gpu:1 --time 0:30:00 bash
```

Inside the session the `SLURM_*` environment is set (the queue's `starter_method`
runs the same fabrication as a batch job), so `srun` launches steps from it:

```bash
srun --pty -p batch -N 2 --ntasks-per-node=1 bash   # a 2-node allocation
# ... then, inside the session:
srun hostname                                        # fans out across both nodes
```

What maps to what: `--partition` -> queue + PE + slots; `-N`/`--ntasks-per-node`
pin the layout via `qsub -par` on OCS 9.1.5+ (as with `sbatch`); `--time` ->
`h_rt`, `--mem` -> the memory complex, `--gres=gpu:N`/`--gpus` -> the GPU complex;
`--account` -> `-A` (surfaces as `SLURM_JOB_ACCOUNT`); `--export` -> `-V`/`-v`.
`--qos` has no Grid Engine analogue and is warned; `--x11`/`--overlap`/`--no-kill`
are accepted and ignored.

Notes and limits:
- The session starts in the directory you ran `srun` from (like SLURM), not `$HOME`.
- `qrsh` uses `-now no`, so the request **queues and waits** for resources rather
  than being rejected immediately -- and it may land on a `BATCH`-type queue.
- `SLURM_SHIM_DRY_RUN` / `--test-only` print the `qrsh` line and create no job.
- No `salloc` / `sattach`: `salloc`'s shell runs on the submit host, which has no
  Grid Engine analogue worth emulating. A non-`--pty` `srun` outside an allocation
  still exits 1.
- GPU sessions see every device on the node under `gpu.isolation: shim`
  (`CUDA_VISIBLE_DEVICES` is set per rank by `srun`, not at the session level);
  run the workload through `srun` inside the session, or use `gpu.isolation: cgroup`.
- On a site whose queue uses a non-builtin interactive daemon (e.g. `sshd -i`) the
  environment still arrives but the session may have no terminal; `srun` warns.

### `sacct` fidelity

`sacct` reports what GE records, not a SLURM accounting database. Five
differences are worth knowing:

- **A selector is required.** Real `sacct` with no arguments reports today's jobs
  for the invoking user; this one prints only the header. Pass `-j`, `-u`, or
  `-S`/`-E`.
- **No job steps.** There is no `<id>.batch` / `<id>.extern` / `<id>.0` row — one
  job is one row. `-X` is accepted and is a no-op for that reason.
- **Array elements are reported by 0-based position.** GE task *k* is reported as
  `<id>_<k-1>`, which is exact for the 0-based arrays submitit and Hydra submit
  but shifts a native `qsub -t 1-3` array to `_0.._2`. Elements of an array that
  has not started yet are not listed at all — `sacct` on a freshly queued array
  returns nothing until its tasks begin.
- **`ExitCode` needs OCS 9.1.5.** On earlier releases a job that exited non-zero
  was reported `COMPLETED` / `0:0`
  ([why](docs/solutions/integration-issues/pe-jobs-lose-exit-status-in-accounting.md)).
- **A `--time` expiry reports `CANCELLED`, not `TIMEOUT`.** GE's execd kills the
  job itself and records the same `failed` code as a `qdel`, which the accounting
  record cannot distinguish. The job *is* killed at the limit, and the state is
  terminal — only the label differs from SLURM.

### Memory requests

`--mem` / `--mem-per-cpu` become `qsub -l <memory_complex>=<n>`, where
`memory_complex` defaults to **`mem_free`**. That default is a deliberate
trade-off, and it is not what a SLURM user expects, so it is worth stating
plainly:

**Under the default, `--mem` is a scheduling filter, not a limit.** The job is
placed on a host reporting that much free memory, and is *not* killed for
exceeding it. SLURM would OOM-kill it at the limit; the shim will not.

The reason is that the obvious enforcing complex is unusable for this shim's
workloads. `h_vmem` is enforced by Grid Engine as a **virtual address space**
cap (`RLIMIT_AS`). Measured on OCS 9.1.5, `-l h_vmem=1G` gives the job
`ulimit -v 1048576`, and any allocation past it fails:

```
dd: memory exhausted by input buffer of size 2147483648 bytes (2.0 GiB)
```

A CUDA context reserves tens of GB of address space at initialisation and
touches almost none of it, so `--mem=16G` with `memory_complex: h_vmem` fails
every PyTorch/JAX process on real GPU hardware, at start, before any of your code
runs. The same applies to the JVM. `sbatch`/`srun` warn if you configure it on a
job that requests GPUs.

`h_rss` is **not** an alternative: it maps to `RLIMIT_RSS`, which Linux has
ignored for two decades (measured: the limit is set, a larger allocation still
succeeds). It enforces nothing.

**If your site wants enforcement**, the options are:

- Make `mem_free` consumable (`qconf -mc`) so requests actually reserve capacity
  instead of only filtering on an instantaneous load value. This is the usual
  fix and costs nothing at the shim level.
- Set `memory_complex: h_vmem` on **CPU-only** sites, where no CUDA context
  exists to be capped. Ensure `h_vmem` is in the hosts' `complex_values`, or
  every `--mem` job becomes unsatisfiable (Grid Engine tries each host, fails,
  and can mark the queue `E` cluster-wide).
- Enforce with cgroups outside the shim. Stock OCS 9.1.5 has no `m_mem_free`
  consumable.

### Known limitations

- No `salloc` / `sattach`; no job-step overlap or heterogeneous steps. Interactive shells use `srun --pty` (above), not `salloc`.
- `sbatch` translates `--time`/`--mem`/`--array`/`--dependency`/`--gpus`/`--gres`/`--signal` (all verified against a live cluster by `test/e2e/31_sbatch_resources.sh`) and pins `--nodes`/`--ntasks-per-node` on OCS 9.1.5+ (`test/e2e/32_par_allocation.sh`); `--exclusive` is warn-and-ignored.
- **A layout Grid Engine cannot grant evenly is not pinned.** A fixed allocation rule puts the *same* slot count on every node, so SLURM's `-N 3 -n 7` (3,2,2) has no faithful translation: the shim warns and lets the PE place the nodes, as it did before.
- **Pinning the layout changes the per-node memory ceiling.** `--mem` is per node on SLURM but the memory complex is per slot on most GE sites, so a job whose 6 slots used to land on one host now gets its grant on each of three. `sbatch` prints a `note:` line with the arithmetic when `--mem` and a pinned rule are both present. Non-contiguous `--array=1-5,20` (comma lists) are rejected — GE arrays are a single contiguous range.
- **Requests that are accepted but cannot be fully honored now warn rather than failing quietly.** `sbatch` warns at submit time for `--exclusive` (a PE property, not a job one) and for every `--dependency` form GE cannot express (see the table above). `srun` warns *on the compute node* — not at submit — when per-task GPU binding was requested under `gpu.isolation: cgroup`, since GE masks devices per job there; use `gpu.isolation: shim` to bind per rank.
- No PMI/PMIx (MPI via the PE's `mpirun`, which is the supported path anyway).
- **Under a dry run `sbatch` prints no `Submitted batch job` line.** A tool that
  parses stdout for a job id (clearml-agent does) gets the predicted environment
  block instead. See [dry run](#dry-run).
- **GPU paths are not validated on real hardware** — the live e2e suite uses a fake RSMAP complex, so device *assignment* is asserted but CUDA/NCCL never runs.
- PyTorch Lightning requires a **homogeneous** allocation (it raises if `SLURM_NTASKS_PER_NODE` is absent with `ntasks>1`); the fabricator warns on non-uniform per-node counts.

## Requirements

- **Open Cluster Scheduler** — end-to-end suite runs green against live OCS **9.0.10** and **9.1.5**; **9.1.5 or newer recommended**.
- Also targets **Gridware Cluster Scheduler** (same lineage); other SGE-compatible variants: untested.
- A **parallel environment** with `control_slaves TRUE` for multi-node jobs (the shim's preflight checks this). On **OCS 9.1.5+** its `allocation_rule` no longer has to match the job shape — the shim overrides it per job — so one PE covers every *placement*. It does not cover every *task policy*: `task_policy` is keyed on the PE, so a site needing both `slot` and `node` semantics still configures one PE per policy. 🚧 A `docs/pe-setup.md` guide is planned; the reference hook scripts are in [`docs/install/`](docs/install/).
- The PE's `start_proc_args` pointed at `slurm-shim-env` (fabricates the environment per job) and the queue's `starter_method` pointed at `slurm-shim-starter` (sources it into every job, so scripts need no edit). The starter runs as the job user for every job in that queue, so the install tree must be root-owned and not group/world-writable. A failing fabrication fails that job with exit 1; it does not error the queue instance. Native Open Cluster Scheduler jobs in the same queue start as they would without the starter: it honours the `SGE_STARTER_SHELL_*` contract the scheduler hands a `starter_method` (`shell_start_mode`, the `-S`/queue shell, login shells), including the `argv[0]=-<shell>` login-shell convention (set through bash's `exec -a`; without bash, `-l` where the shell accepts it).
- The per-job state lives under the job's `$TMPDIR`. Both the fabricator and the hook refuse a `TMPDIR` or state directory that is not the job's own private directory (a co-tenant can pre-create the predictable `/tmp/<job>.<task>.<queue>` path), and the job owner reclaims its `TMPDIR` by stripping group/world write. Setting the queue's `tmpdir` to a directory only root can create entries in removes that exposure entirely.
- Runtime deps: only the GE client tools (`qrsh`, `qstat`, `qsub`, `qdel`, `qconf`, `qmod`, `qacct`, `qhost`) and the config file. The binary is static (CGO off, `osusergo`/`netgo`).

## Configuration

The shim reads a YAML file at `$SLURM_SHIM_CONFIG`, else `/etc/slurm-shim/config.yaml`. Key settings:

```yaml
partitions:                       # SLURM --partition -> GE queue + PE + slots
  gpu:   {queue: gpu.q, pe: gpu.pe, slots: "per-task"}   # per-task = ntasks * cpus_per_task
  batch: {queue: all.q, pe: smp.pe, slots: "16"}          # or a fixed slot count
  # Opt one partition out when its PE's allocation_rule IS the site policy:
  # smp: {queue: all.q, pe: smp.pe, slots: "per-task", allocation_rule_override: never}
default_partition: batch          # sbatch without -p lands here (SLURM's DEFAULT)
allocation_rule_override: auto    # auto (probe qsub for -par) | never | always
pes:
  gpu.pe: {task_policy: gpu}      # gpu | node | slot -> how SLURM_NTASKS is derived
  smp.pe: {task_policy: slot}     # one task per slot (MPI-style)
gpu:
  discovery: qstat-gres           # RSMAP grant -> CUDA_VISIBLE_DEVICES
  isolation: shim                 # shim (per-rank masking) | cgroup (GE devices_allow)
  gres_complex: gpu
  bind: none                      # none (SLURM default: whole grant visible to every
                                  # task) | per-task (split the grant among tasks)
memory_complex: mem_free          # scheduling filter + source for SLURM_MEM_PER_NODE
                                  # ("" disables). See the --mem row: mem_free is
                                  # advisory; h_vmem caps ADDRESS SPACE and breaks CUDA
export_master_addr: false         # set true to publish MASTER_ADDR/MASTER_PORT
launcher: qrsh-inherit            # qrsh-inherit | local (dev/test)
# Fallback when the site cannot wire a queue starter_method: sbatch submits a
# shim-generated wrapper that fabricates, then execs the stored original script
# verbatim. Only covers jobs submitted through this sbatch (not qsub, not
# submitit/Hydra-generated scripts), and the stored original must sit on shared
# storage for the job's lifetime.
wrapper_mode: false               # true -> sbatch injects fabrication itself
wrapper_spool_dir: ""             # where the original + wrapper are stored ("" = next to the script)
```

🚧 A full configuration reference is planned; the authoritative source today is [`internal/config`](internal/config/config.go).

### Environment variables

| Variable | Effect | Off value |
|---|---|---|
| `SLURM_SHIM_CONFIG` | Path to the config file (overrides `/etc/slurm-shim/config.yaml`). | unset |
| `SLURM_SHIM_DRY_RUN` | Report what would happen; change nothing. See below. | anything but `1`/`true`/`yes`/`y`/`on` |
| `SLURM_SHIM_DISABLE` | In-job scrub-only mode: no layout, no `SLURM_*` exports. | **unset only** — any value, including `0`, enables it |
| `SLURM_SHIM_TASK_POLICY` | Per-job override of the PE's `task_policy`. | unset |

> The three do not share a truthiness rule. `SLURM_SHIM_DRY_RUN` allowlists the
> *on* spellings, so anything it does not recognize (including a typo) leaves the
> real behavior in place — the safe direction for a switch whose on-state
> suppresses work. `SLURM_SHIM_DISABLE` keys on non-empty, so `SLURM_SHIM_DISABLE=0`
> **enables** scrub-only mode.

## Dry run

Report what a command would do and change nothing. Nothing is submitted, launched,
cancelled or spooled; the read-only GE clients (`qstat`, `qconf`) still run, so the
report resolves real cluster state where it can.

```bash
SLURM_SHIM_DRY_RUN=1 sbatch train.sh   # session-wide switch
sbatch --test-only train.sh            # per-invocation; also works as an #SBATCH directive
srun --test-only -n 4 hostname
```

`--test-only` is SLURM's own spelling and reaches callers that control only argv or
`#SBATCH` lines (Hydra, submitit, CI templates). Either turns the mode on.

`sbatch` reports the exact `qsub` command line, how the partition, slot rule and
requested geometry were resolved, and **the `SLURM_*` environment the job would
get** — fabricated by the same code the PE hook runs:

```
sbatch: dry run (SLURM_SHIM_DRY_RUN is set) -- no job was submitted or launched

would submit:
  qsub -terse -q gpu.q -pe gpu.pe 32 -par 8 -w e -N llm-train -o 'slurm-$JOB_ID.out' -j y -cwd -V -l h_rt=3600,gpu=2 train.sh

request:
  partition           gpu -> queue gpu.q, pe gpu.pe
  slots               32 (rule "per-task": ntasks 4 x cpus-per-task 8)
  requested geometry  ntasks 4, cpus-per-task 8, nodes 4
  allocation rule     -par 8 (overrides PE gpu.pe's $fill_up); -w e rejects the job at submit if the layout is not schedulable
  gpus per node       2
  script              train.sh (submitted as-is; the PE start_proc_args hook fabricates)
  predicted spread    4 node(s) x 8 slots -- qsub -par 8 pins 8 slot(s) on each of 4 node(s)

job environment (fabricated on the master host when the job starts):
  ...
```

with the environment block itself on **stdout**:

```
SLURM_JOB_ID=<assigned by qsub>
SLURM_JOB_NUM_NODES=4
SLURM_NTASKS=8                 <- the gpu task policy, not the 4 tasks requested
SLURM_NTASKS_PER_NODE=2
SLURM_CPUS_PER_TASK=4
```

That last point is the reason the mode exists. `SLURM_NTASKS` is derived from the
**grant and the PE's `task_policy`**, not from `--ntasks` — as is
`SLURM_CPUS_PER_TASK` — and a partition with a literal `slots` rule ignores the
requested geometry entirely. A dry run shows the number your framework will
actually read.

### Streams and exit codes

The report goes to **stderr**; only the `KEY=VALUE` environment block goes to
**stdout**, so `SLURM_SHIM_DRY_RUN=1 sbatch job.sh 2>/dev/null` yields a diffable
environment prediction and nothing else. For `srun` this matters more than
convention: its stdout is the ranks' own output stream, and a dry run writes
nothing there.

The exit code is the verdict, so the mode works as a gate:

| Code | Meaning |
|---|---|
| 0 | This would run. |
| 1 | The report proved it cannot: an unsatisfiable `task_policy gpu`, a slot count no `allocation_rule` can dispatch. |
| 8 | `srun` only: no usable launcher, or the PE cannot host the step. |

```bash
sbatch --test-only train.sh && sbatch train.sh
```

### What is and is not predicted

Values in `<angle brackets>` come from the real allocation (job id, host names,
device ids, the memory grant). Everything else is exact **for the predicted
spread**.

The spread is the one thing the shim models rather than reuses, so it is where a
prediction can be wrong — **unless an allocation rule was pinned**, in which case
it is not a model at all. When the request states a layout (`--nodes` or
`--ntasks-per-node`) and the cluster supports `-par`, the report shows the emitted
rule and the spread is exact: Grid Engine grants it or refuses the job at submit.

Without a pinned rule the old caveats stand: `$pe_slots`, a fixed
`allocation_rule` and `$round_robin` are decidable and are reported as such;
`$fill_up` is decided at dispatch, and the report says so and falls back to
`--nodes`.

The prediction honors `SLURM_SHIM_TASK_POLICY` and `SLURM_SHIM_DISABLE` from the
submit environment, because `qsub -V` forwards them into the job.

### Other commands

Inside an allocation, `srun` reports where each rank lands, its cpuset and devices,
the `qrsh -inherit` line that would carry its stepper (rendered by the launcher's
own argv builder, with the control-channel token replaced by a placeholder), and
the per-rank environment additions. It reserves no step id, so the step it
describes is the one the next real `srun` creates.

`scancel` and `scontrol requeue` report the `qdel`/`qmod` they would run, on
stderr.

### Limitations

- **`sbatch` prints no `Submitted batch job` line.** A tool that parses stdout for
  a job id (clearml-agent does) gets the environment block instead. Redirect with
  `2>/dev/null` and check for the `Submitted batch job` prefix, or use the exit code.
- **Secret values are redacted.** `--export=ALL,TOKEN=...` renders as
  `-v 'TOKEN=<value>'`; the key is shown, the value never is.
- **The reported command line is for reading, not pasting** — a `--wrap` or
  wrapper-mode submission names a temp directory that only exists at submit time.
- `SLURM_SHIM_DRY_RUN` is scrubbed from the job environment, so it cannot be
  inherited into an allocation and silently turn every `srun` there into a no-op.

## Support

The shim is open source (Apache-2.0) and community-supported via GitHub issues — bug reports with the failing SLURM script attached are the most useful.

*(TODO: confirm internally whether commercial [Gridware Cluster Scheduler](https://hpc-gridware.com/gridware-cluster-scheduler/) support will cover the shim before advertising it here.)*

## Contributing

🚧 `CONTRIBUTING.md` and `good-first-issue` labels are not set up yet. Most future "add support for sbatch flag X" work touches a single mapping table (`internal/cli/sbatch/translate.go`); the `SLURM_*` contract lives in `internal/fabricator`. Run `make test` and `make lint` before submitting.

## Trademarks

Developed by [HPC-Gridware](https://hpc-gridware.com), the company behind Open Cluster Scheduler. This project is not affiliated with or endorsed by SchedMD®. SLURM is a trademark of SchedMD LLC.

## License

Apache-2.0 — see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE). Copyright 2026 HPC-Gridware.
