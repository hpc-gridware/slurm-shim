# Flax on slurm-shim

Data-parallel training across the whole allocation: one global `jax.sharding`
mesh spanning every process's devices, parameters replicated over it, the batch
sharded across it. Each step ends in a gradient all-reduce, and on a multi-node
job that all-reduce is traffic between the nodes.

This is the **workload** counterpart to [`jax/`](../jax/). That recipe proves the
process group forms; this one runs a real optimizer over it, which is a much
harder thing for a broken environment to fake:

| If this is wrong | jax_check.py | flax_dp_train.py |
|---|---|---|
| `SLURM_STEP_NODELIST` | hangs at rendezvous | hangs at rendezvous |
| `SLURM_NTASKS` | reports the mismatch | global/local shape mismatch when the batch is built |
| `SLURM_LOCALID` | -- | two processes claim one device |
| cross-process reduce never happens | -- | per-process weights diverge; **caught** |
| each process feeds the same data | -- | batch checksum mismatch; **caught** |

## Run it

```bash
# From this directory (the job runs in the submit dir, like SLURM).
sbatch flax-multinode.sh         # GPU, multi-node, one process per GPU
sbatch flax-cpu-check.sh         # CPU-only, no GPUs needed
```

- [`flax-multinode.sh`](flax-multinode.sh) -- the standard shape. One task per
  GPU, GPUs requested **per node**.
- [`flax-cpu-check.sh`](flax-cpu-check.sh) -- same code path without devices.
  `JAX_NUM_CPU_DEVICES=2` gives each process two CPU devices so the mesh spans
  devices as well as processes.
- [`flax_dp_train.py`](flax_dp_train.py) -- contains **no SLURM parsing**: a bare
  `jax.distributed.initialize()`, a global mesh over `jax.devices()`, 200 SGD
  steps on a small MLP, and three assertions that only hold if the job really is
  one distributed run.

The one shim-specific line is the hook (`. /opt/slurm-shim/etc/slurm-shim-source-hook.sh`),
which supplies the job-level SLURM environment. Everything else is stock SLURM.

## What the shim provides

Identical to the [`jax/`](../jax/) contract -- `jax.distributed.initialize()`
auto-detects only when **all five** are present, and the shim sets all five:

| JAX reads | Used for | Shim provides |
|---|---|---|
| `SLURM_JOB_ID` | coordinator **port** (`id % 4096 + 61440`) | job-level, from the GE job id |
| `SLURM_STEP_NODELIST` | coordinator **host** (first name in the list) | per step, by `srun` |
| `SLURM_NTASKS` | `jax.process_count()` | per step, the step's task count |
| `SLURM_PROCID` | `jax.process_index()` | per rank |
| `SLURM_LOCALID` | `local_device_ids` | per rank |

Plus per-rank `CUDA_VISIBLE_DEVICES` from the GE RSMAP grant, which is what makes
`jax.local_device_count()` come out right on GPU nodes.

## Pick the right shape (the one thing to get right)

**One process per GPU, and every process must see the node's whole GPU set.**

| You write | Each task sees | Result |
|---|---|---|
| `--ntasks-per-node=4 --gpus-per-node=4` | all 4 devices | correct: 4 processes, 1 device each, 4N-device mesh |
| `--ntasks-per-node=1 --gpus-per-node=4` | all 4 devices | **silently uses 1 of 4 GPUs** (`local_device_ids=[0]`) |
| `--gpus-per-task=1` | 1 device (as index 0) | **`CUDA_ERROR_INVALID_DEVICE` on ranks 1+** |

`--gpus-per-task` does not work here for the same reason it does not work for
JAX: it masks each task down to one device, and JAX then asks for visible index
`SLURM_LOCALID`, which does not exist on ranks above 0. Use `--gpus-per-node` (or
`--gres=gpu:N`). The shim follows SLURM here -- without an explicit binding
request every task sees the node's whole grant.

## How the script is built (and why)

The global batch is **per-device**: `PER_DEVICE_BATCH * jax.device_count()`. Add
nodes and the global batch grows -- ordinary data-parallel semantics, and it
keeps the batch evenly divisible by the mesh no matter how the job is shaped.

Two multi-process details worth copying into real code:

- **Every array entering `jit` must be global.** A plain `jnp.array(...)` is
  committed to one local device and cannot be mixed with sharded inputs.
  `jax.make_array_from_process_local_data(sharding, local_piece)` assembles the
  global array from each process's own slice; under `PartitionSpec()` it is also
  how the replicated parameters are placed.
- **Flax NNX is used functionally.** `nnx.split` separates the static graph
  definition from the parameter pytree; the graphdef is a `jit` constant and the
  parameters are ordinary arrays you can shard and hand to optax. `nnx.merge`
  rebuilds the module inside the loss function.

The verdict line is `FLAX DATA PARALLEL OK` only if all of: the batch checksum
matches the full dataset, every process ends with bit-identical weights, the loss
decreased, and `jax.process_count()` equals `SLURM_NTASKS`. That last one catches
the nastiest failure -- auto-detection not firing and every rank training alone.

## Requirements

- `jax`, `flax`, `optax` importable on every node (a shared venv on the shared
  filesystem, a container image, or a module).
- **JAX >= 0.5.1** for zero-config CPU collectives (gloo became the default);
  >= 0.8.1 recommended, as auto-detection then requires all five variables and a
  partial environment is ignored cleanly instead of raising. Note jax 0.11.1
  needs Python >= 3.12 -- on Python 3.11 pip resolves to 0.10.2, which this
  recipe is verified against as well.
- Each node's own `hostname` must resolve to a routable, non-loopback address --
  CPU/gloo binds to `gethostname()`.
- TCP 61440-65535 reachable node-to-node.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `Unable to initialize backend 'cpu' ... Unable to find address for: <host>` | gloo binds to the node's **own** `gethostname()` | fix `/etc/hosts`/DNS on the node (a hostname mapped only to `127.0.0.1` fails) |
| Hangs in `initialize()` | coordinator host unreachable from a node | check `scontrol show hostnames`; the first name must resolve everywhere |
| `checksum MISMATCH` | the global batch did not contain every process's rows | each process must slice its **own** portion; see `lo = proc * rows_per_proc` |
| weights differ across processes | gradients were not reduced across the group | parameters must be replicated (`PartitionSpec()`), the batch sharded (`PartitionSpec("data")`) |
| `CUDA_ERROR_INVALID_DEVICE` on ranks 1+ | `--gpus-per-task` masked each rank to one device | use `--gpus-per-node` / `--gres=gpu:N` |
| Runs as a single process | no `srun` (a bare `python train.py` has no `SLURM_STEP_NODELIST`) | run under `srun` -- this matches real SLURM and is intentional |
| `Gloo all-reduce failed: Timed out waiting 1800000ms for send operation` after step 0 | JAX dispatches asynchronously; a training loop that never reads a value queues **every** step's collective at once, and CPU/gloo does not survive that depth | read the loss each step (this script does) so exactly one step is in flight; the symptom is a 30-minute hang, not a crash |
| GPU OOM at startup | JAX preallocates ~75% of each device | lower `XLA_PYTHON_CLIENT_MEM_FRACTION` |
| Array tasks join each other's group | all array tasks share one `SLURM_JOB_ID`, so all derive the same port | the script gives each task its own `JAX_COORDINATOR_PORT` |

## Verified live (OCS 9.1.4 and 9.1.5, 3-node test cluster)

`make demo-flax` in [`test/cluster`](../../../test/cluster), jax 0.10.2 /
flax 0.12.8 / optax 0.2.8 on Python 3.11, 22 seconds end to end. First run on
OCS 9.1.4 (2026-08-25), re-run unchanged on OCS 9.1.5 (2026-08-28) with the same
losses to the last digit:

```
[alloc] SLURM_NNODES=3 SLURM_NTASKS=3
[alloc] nodelist=ocs-worker[1-2],ocs-master
[proc 1/3] host=ocs-worker2 local_devices=2 global_devices=6 CUDA_VISIBLE_DEVICES=[unset]
[proc 2/3] host=ocs-master  local_devices=2 global_devices=6 CUDA_VISIBLE_DEVICES=[unset]
[proc 0/3] host=ocs-worker1 local_devices=2 global_devices=6 CUDA_VISIBLE_DEVICES=[unset]
[data] global batch (96, 8) over 6 devices, checksum ok
[train] step    0 loss=4.480681
[train] step   24 loss=0.101037
[verify] weights identical across 3 processes: max|diff|=0.0
[result] FLAX DATA PARALLEL OK
```

One process per node, two CPU devices each, one 6-device mesh spanning all three
containers, gradients reduced across hosts every step.

Also verified off-cluster with jax 0.11.1 / flax 0.12.9 on Python 3.12, driving
the script with exactly the five `SLURM_*` variables the shim fabricates. Shapes
exercised: 3 processes x 2 devices, 3 x 1 (the GPU shape), 2 x 4. The losses are
identical across both jax versions and both platforms, which is why the numbers
above double as a regression baseline.

The negative case was checked too -- with every process slicing the same rows,
training still converges to a healthy-looking loss but the verdict is
`FLAX DATA PARALLEL FAILED` on the checksum, which is the point of having it.
