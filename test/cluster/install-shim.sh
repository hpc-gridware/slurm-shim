#!/usr/bin/env bash
# Install slurm-shim onto the running OCS cluster (docker cp / docker exec only).
#
#   install-shim.sh [--gpu]
#
# --gpu also configures a fake RSMAP GPU complex so the GPU env path can be
# exercised without real hardware.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

want_gpu=0
[ "${1:-}" = "--gpu" ] && want_gpu=1

require_cluster

arch="$(container_arch)"
log "building slurm-shim for linux/$arch"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
( cd "$REPO_ROOT" && GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
    go build -tags osusergo,netgo -trimpath -o "$tmp/slurm-shim" ./cmd/slurm-shim )

# The qrsh envelope carries the master's absolute shim path as the remote argv0
# (REQ-RUN-009), so the binary must live at the SAME path on every node.
cmds=(srun sbatch squeue scancel scontrol sinfo slurm-shim-env slurm-shim-stepper)
for node in "${NODES[@]}"; do
  log "installing shim on $node -> $SHIM_PREFIX"
  docker exec "$node" mkdir -p "$SHIM_PREFIX/bin" "$SHIM_PREFIX/etc" /etc/slurm-shim
  docker cp "$tmp/slurm-shim" "$node:$SHIM_PREFIX/bin/slurm-shim"
  docker exec "$node" bash -c "chmod +x '$SHIM_PREFIX/bin/slurm-shim'; cd '$SHIM_PREFIX/bin'; for c in ${cmds[*]}; do ln -sf slurm-shim \"\$c\"; done"
  docker cp "$REPO_ROOT/docs/install/slurm-shim-source-hook.sh" "$node:$SHIM_PREFIX/etc/slurm-shim-source-hook.sh"
  docker cp "$CLUSTER_DIR/config.yaml" "$node:/etc/slurm-shim/config.yaml"
  # Put the shim commands on PATH for interactive shells. Login shells read
  # /etc/profile.d; interactive non-login shells (docker exec ... bash) read
  # /etc/bash.bashrc.local on openSUSE (see /etc/bash.bashrc). Hook both so
  # `srun`/`sbatch`/... resolve however the user opens a shell.
  docker exec "$node" bash -c "
    printf 'export PATH=%s/bin:\$PATH\n' '$SHIM_PREFIX' > /etc/profile.d/slurm-shim.sh
    grep -qs slurm-shim /etc/bash.bashrc.local 2>/dev/null || \
      printf '. /etc/profile.d/slurm-shim.sh\n' >> /etc/bash.bashrc.local"
done

# PE mode: the 'make' PE runs slurm-shim-env on the master before the job script,
# which fabricates layout.json + the SLURM_* environment file into $TMPDIR.
# qconf mutations require a GE manager (root on this cluster).
log "wiring PE 'make' start_proc_args -> slurm-shim-env"
manager "qconf -mattr pe start_proc_args '$SHIM_PREFIX/bin/slurm-shim-env' make"

# Test-cluster tuning (validated live): Docker containers share the HOST load
# average (loadavg is not namespaced), so GE's default load_thresholds falsely
# drop every queue as "overloaded" whenever the host is busy (e.g. building the
# shim). And a failed job leaves its queue in QERROR, blocking later jobs. Both
# are noise on a throwaway cluster.
log "tuning queues (disable the false load alarm, clear stale QERROR)"
manager "qconf -rattr queue load_thresholds NONE all.q >/dev/null 2>&1 || true"
manager "qmod -c 'all.q@*' >/dev/null 2>&1 || true"

if [ "$want_gpu" = 1 ]; then
  ensure_gpu_complex
fi

log "slurm-shim installed on ${NODES[*]}"
