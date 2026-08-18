# Ray on slurm-shim

Moves Ray (and everything on top of it -- Ray Train, Ray Tune, Ray Serve, and
multi-node vLLM) from **Partial** to **Ready**. Ray does not use SLURM's launcher
or PMI at all; it runs its own head/worker cluster. The scheduler's only job is to
start **one Ray process per node** and tell the workers where the head is. That is
exactly what `srun` does here.

## Run it

```bash
sbatch ray-cluster.sh          # -> Submitted batch job <id>
```

[`ray-cluster.sh`](ray-cluster.sh):

1. Expands the nodelist with `scontrol show hostnames` (master first).
2. Starts `ray start --head` on node 0 via `srun -w <head>`.
3. Starts `ray start --address=<head>:6379` on each remaining node via `srun -w`.
4. Runs the driver on the master (which is the head node), connecting with
   `ray.init(address="auto")` / `RAY_ADDRESS`.
5. `ray stop` on teardown; GE's `qdel` cleanup is the backstop.

Key shim details:

- `--ntasks-per-node=1` -> one Ray process per node, and that process is granted
  the node's **whole** GPU set, so `--num-gpus=$SLURM_GPUS_ON_NODE` is correct and
  Ray schedules tasks/actors across all of them.
- `srun -w <host> --nodes=1 --ntasks=1` targets a specific allocation node (the
  master goes through the local launcher, slaves through `qrsh` tight
  integration). `--block` keeps each Ray process -- and its `srun` step -- alive
  for the life of the job.
- The head IP is resolved with `getent hosts` (the same NSS path the shim uses),
  not left to Ray's autodetection, so workers dial a routable address.

## Multi-node vLLM

vLLM's multi-node tensor/pipeline parallelism requires Ray. Bring the cluster up
exactly as above, then replace the training driver with a `vllm serve` (see the
commented block in the script) sized as tensor-parallel = GPUs/node,
pipeline-parallel = nodes.

## Requirements

- A `gpu` RSMAP complex granting GPUs on each node.
- Inter-node TCP (Ray's object store + gcs) -- already available under GE builtin
  qrsh back-connections.

## Validated on the OCS test cluster (2026-08-18)

The integration was confirmed end to end on the 3-node OCS 9.1.4 test cluster
([`test/cluster`](../../../test/cluster)), CPU-only and GPU-free, with
[`ray-smoke-test.sh`](ray-smoke-test.sh).

Setup (containers ship no Python): installed Python 3.11 on every node and put
Ray in a **shared** venv on the shared home (`/home/gridware/rayenv`), so all
nodes run the same `ray`/`python`:

```bash
# on the master (the venv lives on the shared filesystem)
python3.11 -m venv /home/gridware/rayenv
/home/gridware/rayenv/bin/pip install ray          # ray core; no GPU, no dashboard
RAY_BIN=/home/gridware/rayenv/bin/ray \
PYTHON_BIN=/home/gridware/rayenv/bin/python \
  sbatch ray-smoke-test.sh
```

Result -- a real 3-node cluster formed and ran distributed tasks:

```
[ray] head=ocs-worker2 (10.100.0.12:6379)  nodes=3: ocs-worker2 ocs-worker1 ocs-master
[ray] alive nodes: 1/3  ->  3/3
[driver] alive ray nodes: 3
[driver] cluster CPU: 6.0                     # 2 cpus/node x 3
[driver] task placement across hosts: {'ocs-worker2': 37, 'ocs-worker1': 3}
[driver] RAY MULTI-NODE OK                    # driver rc=0
```

The head started on the job's PE-master node, both workers joined the GCS over
`qrsh -inherit` tight integration (all three node IPs registered), the driver
connected with `ray.init(address="auto")` and saw 3 alive nodes / 6 CPUs, and
remote tasks executed across more than one host. Teardown (`ray stop` + dropping
the worker `srun` steps) left no stray Ray processes.

Notes for a real deployment: `ray` and `python` must resolve on every node (a
shared venv, a container image, or an environment module); the head IP is taken
from `getent hosts` first line (it can return more than one); and this is a
device-visibility / cluster-formation test, not an NCCL/throughput benchmark.
