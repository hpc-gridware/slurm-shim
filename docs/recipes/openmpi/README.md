# openmpi: MPI jobs from SLURM scripts

OpenMPI runs under the shim the way SLURM sites run it: `mpirun` in the batch
script, or `srun --mpi=pmix`. Use this for SLURM scripts and tools that launch
MPI. For Grid Engine scripts, OpenMPI's native Grid Engine integration
(`mpirun` started with `qrsh -inherit`, no shim in between) is the simpler way.

| File | What it is |
|---|---|
| [`hello_mpi.c`](hello_mpi.c) | every rank prints its rank, host and an all-reduce over all ranks (`sum` equals `size` when the job is wired up) |
| [`openmpi.sh`](openmpi.sh) | compiles `hello_mpi.c`, runs it with `mpirun`, then with `srun --mpi=pmix` |
| [`openmpi.def`](openmpi.def) | an Apptainer image with OpenMPI 4.1 and `hello_mpi` |
| [`openmpi-apptainer.sh`](openmpi-apptainer.sh) | the same two runs, each rank in the container |

**Needs:** OpenMPI 4.1 or newer at the same path on every node, with `mpicc`
and `mpirun` on `PATH` (it is built with SLURM support by default), and this
directory on a shared filesystem.

## Run

```bash
sbatch openmpi.sh                                   # 3 nodes x 4 tasks
sbatch --nodes=2 --ntasks-per-node=8 openmpi.sh
```

Neither run needs `-n`: both start one rank per task of the job. Both lines
also take the usual step options (`-n`, `-N`, `-w`, `--exclude`, `-c`).

- **`mpirun`** starts one OpenMPI daemon per node through the shim's `srun`;
  the daemons start the ranks. `SLURM_PROCID` in a rank is its daemon's, as
  under SLURM; use `OMPI_COMM_WORLD_RANK` for the rank.
- **`srun --mpi=pmix`** hands the step to `mpirun` and gives every rank its
  own `SLURM_PROCID`, `SLURM_LOCALID` and `SLURM_NODEID`. The ranks belong to
  `mpirun`: `-l` uses OpenMPI's tag format, and per-rank `-o %t` files,
  `--pty` and spares are refused.

## In an Apptainer container

The host's `mpirun` and daemons start the ranks, and each rank runs inside
the image (Apptainer's "hybrid" model). The OpenMPI in the image must be the
host's release series: 4.1 in [`openmpi.def`](openmpi.def).

```bash
apptainer build openmpi.sif openmpi.def             # as root, or --fakeroot
sbatch openmpi-apptainer.sh                          # IMAGE=/path/to.sif to use another
```

The script sets `OMPI_MCA_btl_vader_single_copy_mechanism=none`: ranks in an
unprivileged container sit in their own user namespace, where OpenMPI's
default shared-memory copy is not allowed, and OpenMPI would warn once per
node. Apptainer binds `/tmp` by default, which the ranks need to reach the
host daemon's PMIx server; keep it bound.

## Verified

On the CPU test cluster: 3 nodes, Open Cluster Scheduler 9.1.6, openSUSE Leap
15.6, Apptainer 1.4.5. `openmpi.sh` ran with OpenMPI 4.1.6 and 5.0.9,
`openmpi-apptainer.sh` with 4.1.6 on the host and in the image. Every job
ended `COMPLETED`. `openmpi-apptainer.sh` printed:

```
== mpirun, ranks in the container
rank=0 size=12 host=ocs-master sum=12 slurm_procid=0
rank=1 size=12 host=ocs-master sum=12 slurm_procid=0
...
rank=8 size=12 host=ocs-worker2 sum=12 slurm_procid=1
...
rank=11 size=12 host=ocs-worker2 sum=12 slurm_procid=1
== srun --mpi=pmix, ranks in the container
rank=0 size=12 host=ocs-master sum=12 slurm_procid=0
rank=1 size=12 host=ocs-master sum=12 slurm_procid=1
...
rank=8 size=12 host=ocs-worker2 sum=12 slurm_procid=8
...
rank=11 size=12 host=ocs-worker2 sum=12 slurm_procid=11
```
