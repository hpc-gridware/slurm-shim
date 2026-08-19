"""Minimal multi-process JAX check.

Nothing here parses SLURM_*: `jax.distributed.initialize()` auto-detects the job
from the five variables the shim fabricates (SLURM_JOB_ID, SLURM_STEP_NODELIST,
SLURM_NTASKS, SLURM_PROCID, SLURM_LOCALID), derives the coordinator address
itself, and forms the process group. We print the topology JAX resolved and run
one collective to prove the group really spans every node.
"""
import os
import socket

import jax
import numpy as np
from jax.experimental import multihost_utils

# Must run before any other JAX call that would initialize a backend.
jax.distributed.initialize()

idx, n = jax.process_index(), jax.process_count()
print(
    f"[proc {idx}/{n}] host={socket.gethostname()} "
    f"local_devices={len(jax.local_devices())} global_devices={len(jax.devices())}",
    flush=True,
)

# Every process contributes its index; the gather must return all of them, which
# is only possible if the group spans the hosts. Numpy input keeps this working
# across versions (JAX 0.8.0 requires tiled=True for non-addressable jax.Arrays).
gathered = multihost_utils.process_allgather(np.array(idx))
seen = sorted(int(v) for v in np.asarray(gathered).reshape(-1))

if idx == 0:
    ok = seen == list(range(n)) and n == int(os.environ["SLURM_NTASKS"])
    print(f"[result] processes seen across the group: {seen}", flush=True)
    print("[result] JAX MULTIPROCESS OK" if ok else "[result] JAX MULTIPROCESS FAILED", flush=True)
