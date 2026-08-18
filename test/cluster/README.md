# Local OCS test cluster

Stand up a real 3-node Open Cluster Scheduler cluster with slurm-shim installed,
for trying the shim out and for version-compat testing. The cluster comes from
the [`quickinstall`](https://github.com/hpc-gridware/quickinstall) repo
**unmodified** — this harness clones it, runs its compose, and layers slurm-shim
on with `docker cp` / `docker exec` only.

## Prerequisites

Docker Engine 20.10+ with Compose v2, ~4 GB RAM, ~10 GB disk, and a Go toolchain
(to build the shim for the container arch).

## Use it

```bash
make cluster-up                 # clone quickinstall, boot cluster (OCS 9.1.4), install shim
make demo                       # multi-node srun fan-out
make demo-gpu                   # per-rank CUDA_VISIBLE_DEVICES from a fake RSMAP grant
make cluster-down               # stop (keeps the OCS install)
make cluster-down ARGS=-v       # stop and wipe the OCS install (needed to change OCS_VERSION)

docker exec -it -u gridware ocs-master bash    # poke around
```

Re-install the shim after a rebuild (cluster already up):

```bash
make cluster-install            # or: ARGS=--gpu to also add the fake RSMAP complex
```

## Knobs

| Env | Default | Meaning |
|-----|---------|---------|
| `OCS_VERSION` | `9.1.4` | which OCS package to install; e.g. `OCS_VERSION=9.0.10 make cluster-up` |
| `QUICKINSTALL_REF` | `main` | which quickinstall commit/branch to run (pin for reproducibility) |
| `QUICKINSTALL_DIR` | *(unset)* | use an existing quickinstall checkout instead of cloning |
| `GPU_PER_WORKER` | `2` | fake RSMAP devices per worker (with `--gpu`) |

To switch OCS version: `make cluster-down ARGS=-v` then `OCS_VERSION=... make cluster-up`.

## How the shim is installed

`install-shim.sh` (idempotent) builds the static binary for the container arch,
`docker cp`s it to `/opt/slurm-shim/bin/slurm-shim` **on every node** (identical
absolute path — the qrsh envelope carries it as the remote argv0), symlinks the
command names, drops `config.yaml` to `/etc/slurm-shim/`, installs the source
hook, and wires the `make` PE's `start_proc_args` to `slurm-shim-env` so PE jobs
fabricate the `SLURM_*` environment. It also disables GE's load alarm on `all.q`
(Docker containers share the host loadavg, which otherwise falsely marks queues
overloaded) and clears any stale `QERROR`.

## Validated end-to-end (OCS 9.1.4, fresh cluster)

`make demo` and `make demo-gpu` were verified on a clean cluster brought up by
this harness:

- **CPU**: `srun` fans 6 ranks across all 3 nodes over `qrsh -inherit` tight
  integration, each with the right `SLURM_PROCID` / `SLURM_LOCALID` /
  `SLURM_NODEID` and host.
- **GPU**: with the fake RSMAP complex, a 2-slot job on one worker lands one
  device per rank (`CUDA_VISIBLE_DEVICES=[0]` and `[1]`).

## Known caveat (long-lived clusters)

On a **long-lived / heavily-experimented** cluster, control_slaves PE jobs
(`qsub -pe make N`, which the shim's `srun` needs) can stay `qw` with *"queue
instance ... temporarily not available"* even though sequential jobs run. That is
a **cluster-side GE scheduling state**, not the shim (the install and the
`slurm-shim-env` fabricator both work). A fresh `make cluster-down ARGS=-v &&
make cluster-up` gives a clean scheduler and is the recommended reset. The
`SLURM_*` fabrication and single-host paths do not depend on it.
