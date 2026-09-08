# Reference install scripts

Three shell files. Each is a delivered artifact (REQ-FAB-001/-009/-010); the
top-level README's Quickstart shows where each goes and the `qconf` lines that
wire them, and `test/cluster/install-shim.sh` performs the whole sequence
against the Docker test cluster.

| File | Installs to | Wired by | Does |
|---|---|---|---|
| `pe-start_proc_args.sh` | (not installed; the PE points straight at the binary) | `qconf -mattr pe start_proc_args /opt/slurm-shim/bin/slurm-shim-env <pe>` | Runs the fabricator on the master host before the job: writes `layout.json` and the `SLURM_*` environment file into the job's `$TMPDIR`, or a failure sentinel. Exits 0 even on failure so a failing `start_proc_args` cannot put the queue instance into error state. |
| `slurm-shim-source-hook.sh` | `<prefix>/etc/slurm-shim-source-hook.sh` (0644, root) | sourced by the starter, or by a job script on a site without one | Reconciles the fabricator's outcome and is the trust boundary for the per-job state: `TMPDIR` and the state directory must be private directories of the job, and the sentinel/environment files private files, or they are reported and ignored (a co-tenant can pre-create the predictable `/tmp/<job>.<task>.<queue>` path). Then: sources the environment file; aborts the job (exit 1) on the sentinel; continues when nothing was fabricated (a native job). `SLURM_SHIM_HOOK_MISSING_ENV=abort` makes the last case fail -- per job only, never cluster-wide. |
| `slurm-shim-starter.sh` | `<prefix>/bin/slurm-shim-starter` (0755, root) | `qconf -mattr queue starter_method /opt/slurm-shim/bin/slurm-shim-starter <queue>` | Open Cluster Scheduler runs every job in the queue through it: sources the hook, then starts the job the way the scheduler would have, honouring `SGE_STARTER_SHELL_START_MODE` / `SHELL_PATH` / `USE_LOGIN_SHELL` (so `posix_compliant`, `script_from_stdin` and login-shell queues keep their semantics; login shells get `argv[0]=-<shell>` via bash's `exec -a`). This is what lets an unmodified SLURM batch script see `SLURM_*`. Passes its own `stepper` launches straight through. Finds the hook at `../etc/slurm-shim-source-hook.sh` relative to itself, so any install prefix works; if it cannot read the hook it says so on stderr rather than running the job silently unconfigured, and `SLURM_SHIM_HOOK_MISSING_ENV=abort` makes that fatal. A non-zero exit fails the job, not the queue instance. |

Everything under the install prefix must be root-owned and not group/world-
writable: the starter runs as the job user, for every job in the queue. The
prefix itself is free -- the starter locates the hook relative to its own path --
but it must be the SAME absolute path on every host, because the qrsh envelope
carries the shim path as the remote argv[0].

Without a `starter_method`, a job script sources the hook itself as its first
line, or the site enables `wrapper_mode` in `config.yaml` so `sbatch` injects
fabrication (covers only jobs submitted through the shim's `sbatch`).

## Shell requirement

`slurm-shim-starter.sh` and `slurm-shim-source-hook.sh` are `#!/bin/sh` scripts
that use `return` inside a sourced file and a `case` on `"$0"` -- POSIX features,
no bashisms. Any POSIX `/bin/sh` satisfies them. **Verified under dash**
(Ubuntu 22.04, `/bin/sh -> /usr/bin/dash`): syntax, the job exec, the stepper
short-circuit and the abort policy all behave identically to bash-as-sh. The
`os-matrix` CI job repeats that on Rocky 8/9 and Ubuntu 22.04/24.04.

## Installing

Use the bootstrap or the tarball; both end in `slurm-shim install`, which is
where the cluster is actually configured (PE, queue wiring, cell config,
firewall rules), idempotently and without prompts. See the README Quickstart.

## Why no rpm or deb yet

The payload is one static binary plus two POSIX shell scripts, and the runtime
tree has to live at `$SGE_ROOT/slurm-shim` -- the one path guaranteed identical
on every node, which the `qrsh` envelope requires. A package cannot put it there
(package paths are fixed; `$SGE_ROOT` varies by site), so it would only stage a
payload somewhere else for `slurm-shim install` to copy: a second location to
explain, for no gain over `tar -xzf`.

This is also how OCS itself arrives -- a tarball unpacked into `$SGE_ROOT` -- so
the shim follows the scheduler it extends rather than introducing a package
manager the scheduler does not use.

**What would change this:** a site that requires signed distro packages, plus
somewhere to publish them and an owner for the core package's weak dependency.
At that point the payload is unchanged and only the wrapper is new; `nfpm` over
`dist/payload` is a small file. Until then, packaging would be four untested
artifacts (rpm x deb x amd64 x arm64) for a project that changes weekly.

## Firewall

Multi-node jobs need two TCP ranges open between the nodes of a job, both
inbound to the job's master node. On a filtered network (GCP VPC, firewalld)
nothing works until a rule admits them, and the failure looks like a hung job
rather than a blocked port.

Run this on a submit host after installing the config, and apply what it prints:

```
/opt/slurm-shim/bin/slurm-shim ports
```

It reads the site's own `config.yaml`, so the ranges it prints are the ones the
shim will actually bind: `control_port_base`/`control_port_range` (default
61000-61439, the control channel) and `master_port_base`/`master_port_range`
(default 20000-29999, the framework rendezvous port).

Keep the rules scoped to the cluster's subnet or tags. The control channel is
token-authenticated, but combined with a spool that leaks the step token to
co-tenants (the SI-51 warning `srun` prints), a world-reachable port is a
needlessly larger target.
