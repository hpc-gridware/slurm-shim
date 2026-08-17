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
