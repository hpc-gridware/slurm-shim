---
title: Per-task GPU masking breaks frameworks that index devices by local rank (JAX, torchrun)
category: integration-issues
problem_type: runtime_error
tags: [gpu, cuda-visible-devices, jax, slurm-parity, gpu-bind, gpus-per-task, local-rank, rsmap, srun]
created: 2026-08-19
date: 2026-08-19
severity: high
component: internal/plan (device assignment), internal/cli/srun, internal/cli/sbatch
---

# Per-task GPU masking breaks frameworks that index devices by local rank

## Problem

A canonical multi-GPU JAX job -- the shape used by NVIDIA JAX-Toolbox/MaxText,
Levanter, NetKet and essentially every published JAX SLURM script:

```bash
#SBATCH --nodes=2
#SBATCH --ntasks-per-node=8     # one task per GPU
#SBATCH --gpus-per-node=8       # note: NOT --gpus-per-task
srun python train.py            # train.py calls jax.distributed.initialize()
```

fails on the shim, on **ranks 1 and above only**:

```
Failed call to cuDeviceGet: CUDA_ERROR_INVALID_DEVICE: invalid device ordinal
RuntimeError: Unable to initialize backend 'cuda': INTERNAL: no supported devices
found for platform CUDA
```

Rank 0 works, which makes it look like a per-node or networking problem rather than
a device-visibility one.

> **Provenance:** this failure is *derived* -- from the per-rank
> `CUDA_VISIBLE_DEVICES` the shim demonstrably emitted plus JAX's documented
> indexing -- and matches the upstream reports linked below. It was **not**
> reproduced on real GPUs; the test cluster is CPU-only with a fake RSMAP complex.
> What was executed is the environment-level assertion (see Verification).

A separate but frequently confused failure is **silent**: with
`--ntasks-per-node=1` on a multi-GPU node the job runs fine and produces plausible
output while using **one GPU out of eight**. That one is caused by fact 1 alone
(JAX pinning `local_device_ids=[0]`) and happens on real SLURM too -- the shim's
masking was never involved, because with a single local task the old code also
handed over the whole grant.

## Root cause

Two facts that only bite when combined.

**1. JAX pins each process to one device, chosen by local rank.** On SLURM+GPU,
`jax.distributed.initialize()` unconditionally sets `local_device_ids =
[SLURM_LOCALID]` (`jax/_src/clusters/cluster.py`), then applies it as a
`jax_cuda_visible_devices` filter (`jax/_src/distributed.py`). Critically, that
index is **into the device list the CUDA runtime enumerates -- i.e. after
`CUDA_VISIBLE_DEVICES` masking**, not a global GPU index. JAX's own docstring says
so: *"defaults to all local devices being visible to the process except when
processes are launched via Slurm and Open MPI on GPUs. In that case, it will
default to a single device per process."*

**2. The shim masked devices per rank when nobody asked it to.**
`internal/plan/gpu.go` divided a node's granted devices among its local ranks
whenever `--gpus-per-task` was absent:

```go
g := gpusPerTask
if g <= 0 {
    g = len(devices) / localTasks   // 8 GPUs / 8 tasks = 1 device per rank
}
```

So each rank received a one-element `CUDA_VISIBLE_DEVICES`, enumerated as ordinal
`0`. Rank with `SLURM_LOCALID=3` then asked JAX for visible index 3, which does not
exist -> `CUDA_ERROR_INVALID_DEVICE`. Rank 0 asked for index 0 and worked by luck.

**Real SLURM does not do this.** Without `--gpus-per-task` or `--gpu-bind`, SLURM
applies no per-task binding and every task sees the job's whole GPU set. Masking is
opt-in: `--gpus-per-task` *implies* `--gpu-bind=per_task:<n>`. Per NERSC's docs:
*"The `--gpus-per-task` option will implicitly set `--gpu-bind=per_task:<gpus_per_task>`
which will restrict GPU access to the tasks which they are bound to."*

The shim's auto-division was therefore a **divergence from SLURM, not a feature** --
and it landed precisely on the one configuration every JAX script uses.

Aggravating factor: `--gpu-bind=none`, the exact incantation the JAX community
circulates as the fix, was **not parsed at all** by the shim. With
`strict_flags: false` it produced only a generic `srun: warning: option
--gpu-bind=none ignored (slurm-shim)` line buried in the job output, so the user
believes they applied the fix while nothing changed.

## Solution

Follow SLURM: bind only when asked.

**1. Default to no binding** (`internal/plan/gpu.go`). Signature gained an
`autoDivide` flag; without an explicit request every local rank sees the whole
grant:

```go
if g <= 0 {
    if !autoDivide {
        // SLURM's default: no binding, the whole grant stays visible.
        for i := range perRank {
            perRank[i] = devices
        }
        return perRank, false
    }
    g = len(devices) / localTasks   // legacy, now opt-in
}
```

**2. Keep the legacy behavior reachable** via `gpu.bind: per-task` in the config
(default `none`), for sites that relied on auto-division.

**3. Parse `--gpu-bind`** (`none`, `per_task[:n]`) on `srun`, and honor it from an
`#SBATCH` directive by publishing `SLURM_GPU_BIND` into the job env -- which is how
real SLURM propagates it.

**4. Translate `sbatch --gpus-per-task`**, which was warn-and-ignored
(`sbatch: warning: unknown directive --gpus-per-task ignored`, because `knownLong`
did not contain it), so a script whose only GPU request was that flag got `qsub`
with **no GPU request at all**. It now emits
`-l <gres> = gpus-per-task * tasks-per-node` and implies per-task binding. Tasks per
node comes from `--ntasks-per-node`, else is derived from `--ntasks`/`--nodes`
(rounding up), else defaults to 1. Note the emitted value's meaning is
site-dependent: on a per-job consumable it is the per-node request, while on a
**per-slot** consumable GE multiplies again by slots (see the JAX recipe's site
prerequisites).

**5. Emit a one-line notice** only in the case whose behavior changed (multi-rank
node, grant >= ranks, and no binding requested either explicitly or by config), so
sites relying on auto-division see a visible change rather than a silent one. An
explicit `--gpu-bind=none` suppresses it -- the user already declined that advice.

### The correct usage, after the fix

| You write | Each task sees | Result |
|---|---|---|
| `--ntasks-per-node=4 --gpus-per-node=4` | all 4 devices | correct: 4 processes, 1 device each |
| `--ntasks-per-node=1 --gpus-per-node=4` | all 4 devices | **silently uses 1 of 4** (`local_device_ids=[0]`) |
| `--gpus-per-task=1` | 1 device (as index 0) | binding requested; wrong for JAX |

## Impact audit (why the default could be flipped safely)

The flip only changes behavior in one case: **more than one task per node AND no
explicit `--gpus-per-task`.** Auditing every shipped GPU consumer showed:

- `deepspeed/`, `ray/`, `lightning/`, `accelerate/`, `submitit/` -- all use one task
  per node, so `localTasks == 1` and they already saw the whole grant. **Unaffected.**
- The recipes README's documented "one task per GPU" model asks for
  `--gpus-per-task=1` **explicitly**, so it still masks. **Unaffected.**
**Affected, and updated with the fix:**

- `test/e2e/60_gpu.sh` -- `srun -n 2` on a 2-GPU node with no bind flag; rewritten
  to assert both models.
- `test/cluster/jobs/demo-gpu.sh` -- same shape; its comment still claimed the shim
  "hands each rank its own CUDA_VISIBLE_DEVICES". Now demonstrates both models.
- `test/cluster/README.md` -- stated a 2-slot job "lands one device per rank".
- `docs/recipes/README.md` -- the two-model GPU section became a three-model one.

**Still to reconcile:** `docs/specs/2026-08-11-slurm-shim-spec-v1.1.md` REQ-GPU-002
normatively states *"Local ranks partition the node's granted device list
contiguously ... Without the flag: `g = floor(gpus/local_tasks)`"* -- the very
requirement `internal/plan/gpu.go` cites while now doing the opposite. The spec is
gitignored (local-only) but is still the normative source and should be amended.

Doing this audit before flipping is what made the change safe, and it is what showed
the config knob does not need to carry the default. The audit's blind spot is worth
noting: it initially covered only *recipes and tests*, and missed the **spec and a
cluster README** that also encode the behavior. When flipping a default, grep for the
behavior's description, not just its call sites.

## Prevention

**Assert the invariant, not the implementation.** The bug is expressible in one
line that is true regardless of binding model:

> every local rank must be a valid index into its own visible device list, i.e.
> `SLURM_LOCALID < len(CUDA_VISIBLE_DEVICES.split(","))`

This is now asserted in `internal/plan/gpu_test.go`, in
`internal/cli/srun/jax_contract_test.go`, and in `test/e2e/60_gpu.sh`. It would have
caught the original bug immediately, and it is framework-agnostic (torch's
`LOCAL_RANK` has the same requirement).

**Reimplement the framework's own logic in a test.** `jax_contract_test.go` contains
a ~10-line port of JAX's coordinator-address parser and asserts the shim's
`SLURM_STEP_NODELIST` satisfies it. This turns "we think JAX will like this" into a
checkable property, and caught a second latent issue: a single-element bracket group
(`n[1]`) is mis-parsed by JAX as `n1]`, causing a silent 300s hang. The shim never
emits that form, and there is now a regression test to keep it that way.

**Substring assertions cannot distinguish `0` from `0,1`.** The pre-existing e2e
check used `assert_contains "$res" "cuda=0"`, which passes under *both* binding
models -- so a model change would produce a confusing half-failure rather than a
clear signal. GPU assertions now use delimited exact matches:

```bash
assert_contains "$res" "default localid=0 cuda=[0,1]" "unbound rank 0 sees the whole grant"
assert_contains "$res" "pertask localid=0 cuda=[0]" "--gpus-per-task binds rank 0 to one device"
```

**Never silently drop a resource request.** `--gpus-per-task` being warn-and-ignored
produced a job with zero GPUs. When a flag requests a *resource*, translate it or
fail loudly; warn-and-ignore is only safe for flags with no resource implication.

## The generalizable lesson

When emulating another scheduler, a "reasonable" default that the real scheduler
does not have is a **divergence**, not an improvement -- because frameworks encode
the real scheduler's behavior, not the reasonable one. Splitting a node's GPUs
evenly among its tasks is defensible in isolation; it is wrong because SLURM does
not do it, and JAX/torch were written against what SLURM does.

Before adding or keeping a convenience behavior in a compatibility layer, ask what
the emulated system actually does in that exact case, and check whether any consumer
depends on the difference.

## Verification

- 21 Go packages green; `test/e2e/60_gpu.sh` extended to 8 checks covering both
  binding models plus the local-rank invariant; full e2e suite green.
- Verified live on the 3-node OCS 9.1.4 test cluster using its fake RSMAP GPU
  complex: unbound ranks each report `cuda=[0,1]` (the whole grant), while
  `--gpus-per-task=1` still yields `cuda=[0]` and `cuda=[1]`.
- Zero-config `jax.distributed.initialize()` forms a 3-node group on the shim
  (`process_allgather` returns `[0, 1, 2]`).

**Not verified:** real GPU device ordinals and NCCL, since the test cluster is
CPU-only with a fake RSMAP complex. JAX's `local_device_ids` indexing is a no-op on
CPU, so the end effect on real hardware is environment-asserted and reasoned, not
executed.

## References

- Recipe: [`docs/recipes/jax/`](../../recipes/jax/) -- usage, troubleshooting table,
  escape hatches (`JAX_COORDINATOR_ADDRESS`, `JAX_COORDINATOR_PORT`,
  `JAX_LOCAL_DEVICE_IDS`)
- Plan: `docs/plans/2026-08-19-feat-jax-support-recipe-and-gpu-parity-plan.md`
- GPU-visibility models: [`docs/recipes/README.md`](../../recipes/README.md)
- JAX source: [`slurm_cluster.py`](https://github.com/jax-ml/jax/blob/main/jax/_src/clusters/slurm_cluster.py),
  [`cluster.py`](https://github.com/jax-ml/jax/blob/main/jax/_src/clusters/cluster.py),
  [`distributed.py`](https://github.com/jax-ml/jax/blob/main/jax/_src/distributed.py)
- Upstream reports: [jax#23452](https://github.com/jax-ml/jax/issues/23452) (per-task
  masking), [jax#16788](https://github.com/jax-ml/jax/issues/16788) (one device per
  host), [jax#29669](https://github.com/jax-ml/jax/issues/29669)
- [NERSC Perlmutter running jobs](https://docs.nersc.gov/systems/perlmutter/running-jobs/)
  -- `--gpu-bind` semantics
