# gpu-check: does every rank get the GPUs it was granted?

This is the first job to run after setting up a GPU partition, and again after any
change to the RSMAP. It needs no Python and no framework, only `nvidia-smi` on
the nodes.

```bash
sbatch gpu-check.sh                                          # 4 nodes x 2 GPUs
sbatch --nodes=2 --ntasks-per-node=4 --gpus-per-node=4 gpu-check.sh
```

Keep `--ntasks-per-node` equal to `--gpus-per-node`, so there is one task per GPU.

[`gpu-check.sh`](gpu-check.sh) prints one `BATCH` line, one `RANK` line per task
and a closing `JOB` line. The job exits 0 only if every check passes, so `sacct`
shows `COMPLETED` or `FAILED`:

| Field | Meaning |
|---|---|
| `given` | `CUDA_VISIBLE_DEVICES`, set by the shim from the Grid Engine RSMAP grant: the GPUs PyTorch, JAX and friends will use |
| `opened` | the GPUs the task can reach, or `error` if `nvidia-smi` failed. `nvidia-smi` ignores `CUDA_VISIBLE_DEVICES`, so with `gpu.isolation: shim` this is every GPU on the node; with `gpu.isolation: cgroup` (Gridware Cluster Scheduler) only the granted ones |
| `ok` | `1` when every device in `given` is reachable: a UUID must appear in `opened`, a numeric id must index into it. With `gpu.isolation: cgroup` the shim sets no mask, so the task must instead reach exactly the node's granted number of GPUs |
| `rocr` | `ROCR_VISIBLE_DEVICES`: stays `unset` on an NVIDIA site |
| `ulimit_v` (BATCH) | the job's address-space limit: must be `unlimited` |
| `JOB` | `ranks` reported / expected, `failed` tasks, and `devices`: distinct (host, device) pairs in `given` / nodes x GPUs per node. `ok=1` needs every task to report `ok=1`, the device count to match (so no device is granted twice), and `ulimit_v` to be `unlimited` |

A healthy result ends with `JOB ranks=8/8 failed=0 devices=8/8 ok=1`. When the job
holds whole nodes (as below), `given` also equals `opened`.

`srun` also prints a one-line notice, such as `srun: notice: all 2 ranks on
<host> see the node's 2 GPUs (SLURM default)`. That is expected here: this
check wants every task to see the node's whole grant.

To compare with the scheduler's side while the job runs:

```bash
qstat -j <job_id> | grep resource_map
```

## Verified on NVIDIA L4

Run on 4 nodes with 2 NVIDIA L4 each (and on 4 nodes with 1 each), on Rocky
Linux 9 with Open Cluster Scheduler 9.1.5. The RSMAP used device UUIDs as ids
and `gpu.bind: none`, so each task sees its node's whole grant. Host names and
UUIDs are anonymised. That run printed the lines below; the `ok` field, the
`JOB` line and the exit code were added afterwards and checked on the CPU test
cluster with a stand-in `nvidia-smi`:

```
RANK host=gpu-node1 localid=0 procid=0 given=[GPU-uuid-a,GPU-uuid-b] rocr=[unset] opened=[GPU-uuid-a,GPU-uuid-b] n_open=2
RANK host=gpu-node1 localid=1 procid=1 given=[GPU-uuid-a,GPU-uuid-b] rocr=[unset] opened=[GPU-uuid-a,GPU-uuid-b] n_open=2
RANK host=gpu-node4 localid=1 procid=3 given=[GPU-uuid-c,GPU-uuid-d] rocr=[unset] opened=[GPU-uuid-c,GPU-uuid-d] n_open=2
RANK host=gpu-node4 localid=0 procid=2 given=[GPU-uuid-c,GPU-uuid-d] rocr=[unset] opened=[GPU-uuid-c,GPU-uuid-d] n_open=2
RANK host=gpu-node3 localid=1 procid=5 given=[GPU-uuid-e,GPU-uuid-f] rocr=[unset] opened=[GPU-uuid-e,GPU-uuid-f] n_open=2
RANK host=gpu-node3 localid=0 procid=4 given=[GPU-uuid-e,GPU-uuid-f] rocr=[unset] opened=[GPU-uuid-e,GPU-uuid-f] n_open=2
RANK host=gpu-node2 localid=0 procid=6 given=[GPU-uuid-g,GPU-uuid-h] rocr=[unset] opened=[GPU-uuid-g,GPU-uuid-h] n_open=2
RANK host=gpu-node2 localid=1 procid=7 given=[GPU-uuid-g,GPU-uuid-h] rocr=[unset] opened=[GPU-uuid-g,GPU-uuid-h] n_open=2
```

The eight granted devices matched the scheduler's `resource_map` exactly. The
`BATCH` line's two values were verified in separate L4 jobs on the same stack:
- a batch script without `srun` received its node's granted UUID in
  `CUDA_VISIBLE_DEVICES` (shim v0.6.0 and later);
- `ulimit -v` was `unlimited` under `--mem=4G`, where a 2 GiB CUDA allocation
  succeeded.

Not verified yet: `gpu.bind: per-task`, where each task's `given` is its own
share of the node, and `gpu.isolation: cgroup`.

## When it looks wrong

| Symptom | Likely cause | Fix |
|---|---|---|
| `sbatch` refuses the job, or it never leaves `qw` | The GPU complex counts per slot (`consumable YES`), so the per-node request is multiplied by the slot count | Make it `consumable HOST` (`qconf -sc`, `qconf -mc`) |
| `given=[unset]` with `ok=0` | The GPU complex is not an RSMAP, so there are no device ids to hand out | Define the complex as RSMAP with one id per device ([Configuration](../../../README.md#configuration)). With `gpu.isolation: cgroup`, `given=[unset]` is expected |
| `opened=[error]` | `nvidia-smi` is missing or cannot reach the driver | Install or repair the NVIDIA driver on that node |
| Wrong device picked with numeric RSMAP ids | CUDA numbers GPUs fastest-first, `nvidia-smi` numbers them by PCI bus | Use device UUIDs as RSMAP ids, or set `CUDA_DEVICE_ORDER=PCI_BUS_ID` |
| `opened` lists more devices than `given` | Expected with `gpu.isolation: shim` when the job does not hold the whole node: the mask is advisory | For enforcement use `gpu.isolation: cgroup` (Gridware Cluster Scheduler) |
| `ulimit_v` is a number | `--mem` maps to `h_vmem`, or the queue sets `h_vmem`: CUDA dies at start-up | Use a non-limiting memory complex ([Memory requests](../../../README.md#memory-requests)) |

AMD sites (`gpu.vendor: amd`) get `ROCR_VISIBLE_DEVICES` instead. This script is
NVIDIA-only.
