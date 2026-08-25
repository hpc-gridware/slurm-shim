"""Data-parallel Flax training over a multi-process JAX mesh.

Nothing here parses SLURM_*: `jax.distributed.initialize()` auto-detects the job
from the five variables the shim fabricates (SLURM_JOB_ID, SLURM_STEP_NODELIST,
SLURM_NTASKS, SLURM_PROCID, SLURM_LOCALID) and forms the process group.

The difference from jax_check.py next door is what happens after that. This
builds *global* arrays spanning every process's devices and runs a real
optimizer over them, so a wrong environment cannot pass quietly:

  wrong SLURM_NTASKS       -> global/local shape mismatch when the batch is built
  wrong SLURM_STEP_NODELIST-> hang at the coordinator rendezvous
  wrong SLURM_LOCALID      -> two processes claim one device
  no cross-process reduce  -> per-process weights diverge (checked at the end)

Shape: parameters replicated over the mesh, the batch sharded over it. XLA
inserts the gradient all-reduce, and on a multi-node job that all-reduce is
network traffic between the nodes -- the thing worth proving.
"""

import os
import socket

import jax
import jax.numpy as jnp
import numpy as np
import optax
from flax import nnx
from jax.experimental import multihost_utils
from jax.sharding import NamedSharding, PartitionSpec

# Must run before any other JAX call that would initialize a backend.
jax.distributed.initialize()

# At least two steps, so the "loss decreased" check below compares two different
# steps rather than step 0 with itself.
STEPS = max(2, int(os.environ.get("STEPS", "200")))
PER_DEVICE_BATCH = int(os.environ.get("PER_DEVICE_BATCH", "16"))
FEATURES, HIDDEN, SEED = 8, 32, 0

proc, nproc = jax.process_index(), jax.process_count()
local_devices, global_devices = jax.local_device_count(), jax.device_count()
print(
    f"[proc {proc}/{nproc}] host={socket.gethostname()} "
    f"local_devices={local_devices} global_devices={global_devices} "
    f"CUDA_VISIBLE_DEVICES=[{os.environ.get('CUDA_VISIBLE_DEVICES', 'unset')}]",
    flush=True,
)

# The row map below gives every process the same number of rows, so refuse a
# ragged job rather than mis-shard it. Uneven device counts mean a node was given
# a different number of GPUs than its peers (or a different --ntasks-per-node).
counts = np.asarray(multihost_utils.process_allgather(np.array(local_devices))).reshape(-1)
if counts.min() != counts.max():
    raise SystemExit(
        f"[proc {proc}] devices per process are uneven ({list(counts)}); this script "
        "assumes a uniform allocation -- ask for the same devices on every node"
    )

# One mesh axis over every device in the job -- not just this process's. Sharding
# a batch over it is what turns N independent processes into one training run.
mesh = jax.make_mesh((global_devices,), ("data",))
sharded = NamedSharding(mesh, PartitionSpec("data"))
replicated = NamedSharding(mesh, PartitionSpec())

# The dataset is generated identically on every process (same seed, no I/O), and
# each process keeps only the contiguous slice its own devices hold. Global
# device order groups by process, so process p owns rows [p*rows_per_proc, ...).
rows_per_proc = PER_DEVICE_BATCH * local_devices
global_rows = PER_DEVICE_BATCH * global_devices
rng = np.random.default_rng(SEED)
w_true = rng.normal(size=(FEATURES, 1))
x_all = rng.normal(size=(global_rows, FEATURES))
y_all = x_all @ w_true + 0.1 * rng.normal(size=(global_rows, 1))
lo = proc * rows_per_proc
x_local, y_local = x_all[lo : lo + rows_per_proc], y_all[lo : lo + rows_per_proc]

# make_array_from_process_local_data assembles one global array from each
# process's local piece. It is also how the replicated parameters are placed:
# under PartitionSpec() every process contributes the whole array.
x = jax.make_array_from_process_local_data(sharded, x_local)
y = jax.make_array_from_process_local_data(sharded, y_local)


class MLP(nnx.Module):
    def __init__(self, din, dhid, dout, *, rngs):
        self.hidden = nnx.Linear(din, dhid, rngs=rngs)
        self.out = nnx.Linear(dhid, dout, rngs=rngs)

    def __call__(self, batch):
        return self.out(nnx.relu(self.hidden(batch)))


# Split the module into a static graph definition and a parameter pytree: the
# graphdef is a jit constant, the parameters are ordinary arrays we can shard.
graphdef, params = nnx.split(MLP(FEATURES, HIDDEN, 1, rngs=nnx.Rngs(SEED)))
params = jax.tree.map(
    lambda p: jax.make_array_from_process_local_data(replicated, np.asarray(p)), params
)
optimizer = optax.sgd(0.05)
opt_state = optimizer.init(params)


@jax.jit
def train_step(params, opt_state, x, y):
    def loss_fn(p):
        return jnp.mean((nnx.merge(graphdef, p)(x) - y) ** 2)

    loss, grads = jax.value_and_grad(loss_fn)(params)
    updates, opt_state = optimizer.update(grads, opt_state)
    return optax.apply_updates(params, updates), opt_state, loss


# Every row of the global batch must be present, or the mesh did not really span
# the processes. Comparing against the numpy sum every process computed locally
# catches a batch assembled from one process's data only.
checksum = float(jax.jit(jnp.sum)(x))
data_ok = np.isclose(checksum, float(x_all.sum()), rtol=1e-5)
if proc == 0:
    print(
        f"[data] global batch {x.shape} over {global_devices} devices, "
        f"checksum {'ok' if data_ok else 'MISMATCH'}",
        flush=True,
    )

first_loss = last_loss = None
for step in range(STEPS):
    params, opt_state, loss = train_step(params, opt_state, x, y)
    # Keep exactly one step in flight. JAX dispatches asynchronously, so without
    # this the loop queues every step's cross-process all-reduce at once, and CPU
    # gloo does not survive that depth -- it fails as a 30-minute
    # "Timed out waiting ... for send operation to complete", not as a crash.
    # Reading the loss each step is what a real training loop does anyway.
    loss_value = float(loss)
    if step == 0:
        first_loss = loss_value
    last_loss = loss_value
    if proc == 0 and (step % 50 == 0 or step == STEPS - 1):
        print(f"[train] step {step:4d} loss={loss_value:.6f}", flush=True)

# The decisive check. Each process trained on a *different* slice of the data, so
# the only way every process can end with the same weights is if the gradient was
# reduced across the whole group. A few values from *every* leaf, so no single
# parameter that happens to stay put can make a diverged run look converged.
# Numpy input keeps process_allgather working across versions (JAX 0.8.0 requires
# tiled=True for non-addressable jax.Arrays).
sample = np.concatenate(
    [np.asarray(jax.device_get(v)).reshape(-1)[:4] for v in jax.tree.leaves(params)]
)
gathered = np.asarray(multihost_utils.process_allgather(sample))
max_diff = float(np.abs(gathered - gathered[0]).max())

if proc == 0:
    want_ntasks = os.environ.get("SLURM_NTASKS")
    ok = (
        data_ok
        and max_diff == 0.0
        and last_loss < first_loss
        and gathered.shape[0] == nproc
        and (want_ntasks is None or nproc == int(want_ntasks))
    )
    print(f"[verify] weights identical across {nproc} processes: max|diff|={max_diff}", flush=True)
    print(f"[verify] loss {first_loss:.6f} -> {last_loss:.6f}", flush=True)
    print("[result] FLAX DATA PARALLEL OK" if ok else "[result] FLAX DATA PARALLEL FAILED", flush=True)
