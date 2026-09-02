# JAX on slurm-shim

**JAX is the cleanest fit of any stack in these recipes.** It does not use SLURM's
launcher, and it does not need PMI/PMIx (the shim's one hard "no"). It runs its own
gRPC coordination service and asks the scheduler for exactly **five environment
variables**. Your existing SLURM job script runs unchanged:

```bash
#SBATCH --nodes=2
#SBATCH --ntasks-per-node=4     # = GPUs per node
#SBATCH --gpus-per-node=4
srun python train.py            # train.py calls jax.distributed.initialize()
```

## What JAX reads / what the shim provides

`jax.distributed.initialize()` auto-detects SLURM only when **all five** are
present (`SlurmCluster.is_env_present`), and the shim sets all five:

| JAX reads | Used for | Shim provides |
|---|---|---|
| `SLURM_JOB_ID` | coordinator **port** (`id % 4096 + 61440`) | job-level, from the GE job id |
| `SLURM_STEP_NODELIST` | coordinator **host** (first name in the list) | per step, by `srun` |
| `SLURM_NTASKS` | `jax.process_count()` | per step, the step's task count |
| `SLURM_PROCID` | `jax.process_index()` | per rank |
| `SLURM_LOCALID` | `local_device_ids` | per rank |

Two consequences worth internalizing:

- **The coordinator runs in process 0**, and JAX finds it by taking the *first*
  host out of the compressed nodelist. The shim guarantees rank 0 sits on that
  first host (its nodelist encoder preserves allocation order and ranks are
  block-distributed), so the rendezvous lands where it should. This is asserted by
  a unit test that reimplements JAX's parser, and verified live.
- **`SLURM_STEP_NODELIST` only exists under `srun`.** A bare `python train.py` in a
  batch script therefore does *not* auto-detect and runs as a single process --
  exactly as on real SLURM. That is intentional; do not "fix" it.

## Run it

```bash
# From this directory (the job runs in the submit dir, like SLURM).
sbatch jax-multinode.sh          # GPU, multi-node
sbatch jax-cpu-check.sh          # CPU-only verification, no GPUs needed
```

- [`jax-multinode.sh`](jax-multinode.sh) -- the standard shape. One task per GPU,
  GPUs requested **per node**.
- [`jax-cpu-check.sh`](jax-cpu-check.sh) -- same code path without devices; useful
  to validate a cluster before spending GPU time.
- [`jax_check.py`](jax_check.py) -- contains **no SLURM parsing**: a bare
  `jax.distributed.initialize()`, a topology banner, and a `process_allgather`
  that only succeeds if the group really spans the nodes.

The one shim-specific line is the hook (`. /opt/slurm-shim/etc/slurm-shim-source-hook.sh`),
which supplies the job-level SLURM environment. Everything else is stock SLURM.

## Pick the right shape (the one thing to get right)

**JAX wants one process per GPU, and every process must see the node's whole GPU
set.** This is *the opposite* of the one-task-per-node shape the torchrun-based
recipes here use, and it is not a style preference: on SLURM+GPU JAX always sets
`local_device_ids=[SLURM_LOCALID]` and indexes into the process's visible device
list.

| You write | Each task sees | JAX result |
|---|---|---|
| `--ntasks-per-node=4 --gpus-per-node=4` | all 4 devices | correct: 4 processes, 1 device each |
| `--ntasks-per-node=1 --gpus-per-node=4` | all 4 devices | **silently uses 1 of 4 GPUs** (`local_device_ids=[0]`) |
| `--gpus-per-task=1` | 1 device (as index 0) | **`CUDA_ERROR_INVALID_DEVICE` on ranks 1+** |

**What does NOT work:** `--gpus-per-task` for JAX. It binds each task to one
device, and rank 1 then asks for visible index 1, which does not exist. Use
`--gpus-per-node` (or `--gres=gpu:N`) instead. This is a JAX/SLURM interaction,
not a shim quirk -- the same script misbehaves on real SLURM, which is why the
JAX community circulates `--gpu-bind=none` as the fix (the shim accepts that flag
too).

The shim follows SLURM here: **without an explicit binding request every task sees
the node's whole grant.** A site that wants the shim's older behavior (splitting
the grant evenly among tasks) can set `gpu.bind: per-task` in the config, or pass
`--gpu-bind=per_task` per step.

## How to check it worked

The most common failure is not a crash -- it is a job that runs while each node
computes independently, or uses one GPU out of eight. Always print the topology:

```python
import jax
jax.distributed.initialize()
print(f"[{jax.process_index()}/{jax.process_count()}] "
      f"local={jax.local_devices()} global={len(jax.devices())}", flush=True)
```

Check that:

- the `[i/N]` lines cover every `i` from `0` to `N-1`, and `N` equals your total
  task count;
- `len(jax.devices())` equals *GPUs per node x nodes* -- not just the local count;
- `jax.local_devices()` shows the devices you expect on each process.

## Validated on the OCS test cluster (2026-08-19)

Confirmed on the 3-node OCS 9.1.4 cluster ([`test/cluster`](../../../test/cluster))
with **jax 0.10.2** (aarch64 CPU wheel), using a bare `jax.distributed.initialize()`
with no arguments and no JAX-specific configuration:

```
[proc 0/3] host=ocs-worker1 local_devices=1 global_devices=3
[proc 1/3] host=ocs-worker2 local_devices=1 global_devices=3
[proc 2/3] host=ocs-master  local_devices=1 global_devices=3
[Gloo] Rank 0 is connected to 2 peer ranks. Expected number of connected peer ranks is : 2
[result] processes seen across the group: [0, 1, 2]
[result] JAX MULTIPROCESS OK
```

JAX auto-detected the job, resolved the coordinator, and formed **one group across
three hosts** -- the `process_allgather` returning `[0, 1, 2]` is only possible if
the processes genuinely reached each other. The captured environment shows why the
rendezvous landed correctly:

```
SLURM_STEP_NODELIST=[ocs-worker1,ocs-master,ocs-worker2]   rank 0 on ocs-worker1
```

The nodelist is in allocation order (neither sorted nor master-first), and rank 0
is on its first entry -- which is exactly the host JAX parses out.

The **binding models** were verified against the cluster's RSMAP GPU complex:
unbound ranks each see `cuda=[0,1]` (the node's whole grant), while
`--gpus-per-task=1` still yields `cuda=[0]` and `cuda=[1]`.

> **CPU-only caveat.** This cluster has no GPUs, so what is proven here is
> auto-detection, coordinator resolution, group formation, cross-host collectives,
> and the exact per-rank `CUDA_VISIBLE_DEVICES` strings via the fake RSMAP complex.
> **Not** exercised: NCCL, real device ordinals, and JAX's
> `local_device_ids` indexing (a no-op on CPU). Treat the GPU claims above as
> reasoned and environment-asserted rather than executed.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `jax.devices()` shows one device per node instead of one per GPU | one task per node; JAX pinned `local_device_ids=[0]` | set `--ntasks-per-node` = GPUs per node |
| `CUDA_ERROR_INVALID_DEVICE` on ranks 1+ | tasks bound to one device each | drop `--gpus-per-task`; use `--gpus-per-node`, or `--gpu-bind=none` |
| Silent hang, fails after 300s | coordinator port unreachable between nodes | open TCP 61440-65535 node-to-node; raise `initialization_timeout` |
| Hang with a proxy configured | gRPC routes the rendezvous through the proxy | `unset http_proxy https_proxy` (the recipe does this) |
| Two concurrent jobs fight over the coordinator socket | job ids differ by a multiple of 4096 | `export JAX_COORDINATOR_PORT=<free port>` (JAX >= 0.8.1) |
| JAX ignores the SLURM env entirely | something in the job env sets `OMPI_MCA_orte_hnp_uri` -- **JAX checks Open MPI before SLURM** and silently wins | do not launch JAX under `mpirun`; or force `jax.distributed.initialize(cluster_detection_method="slurm")` |
| `Unable to initialize backend 'cpu' ... Unable to find address for: <host>` | CPU/gloo binds to the node's **own** `gethostname()`, which must resolve to a routable address | fix `/etc/hosts`/DNS on the node (a hostname mapped only to `127.0.0.1` fails) |
| Array tasks join each other's group | all array tasks share one `SLURM_JOB_ID`, so all derive the same port | give each task its own port (the recipe does this from `SLURM_ARRAY_TASK_ID`) |
| Coordinator host unresolvable | hostnames like `node01-ib` compress to `node[01-02]-ib`; JAX's parser drops the suffix | `export JAX_COORDINATOR_ADDRESS="$(scontrol show hostnames "$SLURM_STEP_NODELIST" \| head -1):61440"` |
| `DEADLINE_EXCEEDED: Barrier timed out` | fewer processes called `initialize()` than `SLURM_NTASKS` | check the geometry; look for an earlier crash on another rank |
| `KeyError: 'SLURM_NTASKS'` | running outside `srun` (e.g. an ssh'd shell) | run under `srun`, or pass the arguments explicitly |
| `initialize() must be called before any JAX calls` | something touched JAX first | move `initialize()` above the other imports |
| Job never leaves `qw` | `-l gpu=N` against a **per-slot** consumable requests `N x slots` | see the prerequisites below |

## Site prerequisites

- A partition whose PE places the tasks you ask for (one per GPU). On **OCS
  9.1.5+** the shim pins that placement per job (`qsub -par`), so the PE's own
  `allocation_rule` no longer has to match the job shape and `batch`/`gpu` work for
  both single- and multi-node jobs. Below 9.1.5 the PE still decides: single-node
  jobs then want a `$pe_slots` PE such as `smp`. Note the shim pins **slots** per
  node -- `--ntasks-per-node=4 --cpus-per-task=8` is 32 slots per node -- so the
  geometry has to fit a real node either way.
- The GPU complex (`gpu.gres_complex`) and whether it is a **per-slot** or
  **per-job** consumable -- a JAX job asks for many slots *and* N GPUs per node,
  which is exactly where the two differ. Check with `qconf -sc`.
- **Two independent name-resolution requirements**: (1) the host JAX parses out of
  the nodelist must resolve *from every node* (the coordinator), and (2) each
  node's **own** `hostname` must resolve locally to a routable, non-loopback
  address -- CPU/gloo binds to `gethostname()` and there is no JAX-level override.
- TCP 61440-65535 reachable node-to-node.
- **JAX version**: >= 0.5.1 for zero-config CPU collectives (gloo became the
  default), and >= 0.8.1 recommended -- from that release auto-detection requires
  all five variables (so a partial environment is ignored cleanly instead of
  raising `KeyError`) and `JAX_COORDINATOR_PORT` exists. jax 0.11.1 needs
  Python >= 3.12.
- On shared GPU nodes prefer `gpu.isolation: cgroup`, and keep
  `XLA_PYTHON_CLIENT_MEM_FRACTION` in mind: JAX preallocates most of each visible
  device.

## Escape hatches

All read by `jax.distributed.initialize()` before auto-detection, so they win:

- `JAX_COORDINATOR_ADDRESS=host:port` -- bypass nodelist parsing entirely.
- `JAX_COORDINATOR_PORT=<port>` -- keep the derived host, override the port.
- `JAX_LOCAL_DEVICE_IDS=0,1,...` -- override which local devices this process uses
  (verified to take precedence over the SLURM auto-detect path in jax 0.10.2).
- Or pass `coordinator_address=` / `num_processes=` / `process_id=` /
  `local_device_ids=` explicitly to `initialize()`.
