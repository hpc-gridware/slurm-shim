#!/usr/bin/env bash
# Shared helpers for the OCS test-cluster harness.
#
# The cluster itself comes from the quickinstall repo (unmodified); this harness
# clones it on demand, runs its compose, and layers slurm-shim on top with
# docker cp / docker exec only. Nothing here edits a quickinstall file.
set -euo pipefail

CLUSTER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$CLUSTER_DIR/../.." && pwd)"

# --- knobs (override via env) ------------------------------------------------
# QUICKINSTALL_REF pins the cluster TOOLING (compose/installer); OCS_VERSION (a
# passthrough their compose already honors) pins the OCS PACKAGE. They are
# orthogonal: bump the ref for latest tooling, set OCS_VERSION for a given OCS.
# OCS_VERSION defaults to 9.1.4 so the harness is deterministic (not "whatever
# quickinstall's latest happens to be"); override e.g. OCS_VERSION=9.0.10.
QUICKINSTALL_REPO="${QUICKINSTALL_REPO:-https://github.com/hpc-gridware/quickinstall.git}"
QUICKINSTALL_REF="${QUICKINSTALL_REF:-main}"
QUICKINSTALL_DIR="${QUICKINSTALL_DIR:-}"   # use an existing checkout instead of cloning
OCS_VERSION="${OCS_VERSION:-9.1.4}"        # default OCS package version
READY_TIMEOUT="${READY_TIMEOUT:-360}"

SHIM_PREFIX=/opt/slurm-shim               # identical absolute path on every node
COMPOSE_SUBDIR=containers/openSUSE/15.6
NODES=(ocs-master ocs-worker1 ocs-worker2)
MASTER=ocs-master
GPU_COMPLEX="${GPU_COMPLEX:-gpu}"
GPU_PER_WORKER="${GPU_PER_WORKER:-2}"     # fake RSMAP device count per worker

log() { printf '\033[1;36m==>\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# qi_dir prints the quickinstall compose directory (no network).
qi_dir() {
  if [ -n "$QUICKINSTALL_DIR" ]; then
    if [ -f "$QUICKINSTALL_DIR/docker-compose.yml" ]; then echo "$QUICKINSTALL_DIR"
    else echo "$QUICKINSTALL_DIR/$COMPOSE_SUBDIR"; fi
    return
  fi
  echo "$CLUSTER_DIR/.quickinstall/$COMPOSE_SUBDIR"
}

# qi_ensure clones or updates the quickinstall checkout (network). Skipped when
# QUICKINSTALL_DIR points at an existing checkout.
qi_ensure() {
  [ -n "$QUICKINSTALL_DIR" ] && { [ -d "$(qi_dir)" ] || die "QUICKINSTALL_DIR set but $(qi_dir) not found"; return; }
  local clone="$CLUSTER_DIR/.quickinstall"
  if [ -d "$clone/.git" ]; then
    log "updating quickinstall ($QUICKINSTALL_REF)"
    git -C "$clone" fetch -q --depth 1 origin "$QUICKINSTALL_REF"
    git -C "$clone" checkout -q --detach FETCH_HEAD
  else
    log "cloning quickinstall@$QUICKINSTALL_REF"
    git clone -q --depth 1 --branch "$QUICKINSTALL_REF" "$QUICKINSTALL_REPO" "$clone" 2>/dev/null \
      || git clone -q --depth 1 "$QUICKINSTALL_REPO" "$clone"   # ref may be a sha, not a branch
    [ "$QUICKINSTALL_REF" = main ] || git -C "$clone" fetch -q --depth 1 origin "$QUICKINSTALL_REF" && git -C "$clone" checkout -q --detach FETCH_HEAD 2>/dev/null || true
  fi
  [ -f "$(qi_dir)/docker-compose.yml" ] || die "no docker-compose.yml under $(qi_dir)"
}

# compose runs `docker compose` in the quickinstall dir with OCS_VERSION passed
# through (defaults to 9.1.4; see knobs).
compose() {
  ( cd "$(qi_dir)" && OCS_VERSION="$OCS_VERSION" docker compose "$@" )
}

require_cluster() {
  for n in "${NODES[@]}"; do
    docker inspect -f '{{.State.Running}}' "$n" 2>/dev/null | grep -qx true \
      || die "container $n is not running (run: make cluster-up)"
  done
}

# wait_ready blocks until all three nodes have an all.q instance (per the
# quickinstall README: qhost alone is not enough, qstat -f is the real signal).
wait_ready() {
  log "waiting for the cluster (all three all.q instances)"
  local deadline=$((SECONDS + READY_TIMEOUT)) n
  while :; do
    n="$(docker exec "$MASTER" bash -lc 'qstat -f 2>/dev/null | grep -c "^all\.q@"' 2>/dev/null || echo 0)"
    [ "${n:-0}" = 3 ] && { log "cluster ready"; return 0; }
    [ $SECONDS -lt $deadline ] || die "cluster not ready within ${READY_TIMEOUT}s (have $n/3 queue instances)"
    sleep 5
  done
}

# container_arch maps the master's uname -m to a GOARCH.
container_arch() {
  case "$(docker exec "$MASTER" uname -m 2>/dev/null)" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64)  echo amd64 ;;
    *) die "unsupported container arch" ;;
  esac
}

# gridware runs a login shell command as the gridware user on the master (OCS env
# sourced via /etc/profile.d). Login shell is non-interactive so no banner prints.
# cd first: docker exec starts in the image workdir (/), and with the shim's
# SLURM-parity default (jobs run in the submit dir, -cwd) submitting from an
# unwritable cwd would fail output-file creation -- as it would on real SLURM.
gridware() { docker exec -u gridware "$MASTER" bash -lc "cd && $*"; }

# manager runs a command as root on the master. On this cluster root is the only
# GE manager, so qconf mutations (PE hooks, complexes) must go through it.
manager() { docker exec "$MASTER" bash -lc "$*"; }

# ensure_gpu_complex configures a fake RSMAP GPU complex so the GPU path can be
# exercised without real hardware. Idempotent: adds the complex if absent and sets
# GPU_PER_WORKER devices on every non-master node. Shared by install-shim.sh
# (--gpu) and the e2e gpu check.
ensure_gpu_complex() {
  log "ensuring fake RSMAP complex '$GPU_COMPLEX' ($GPU_PER_WORKER/worker)"
  # qconf -Mc replaces the whole complex list, so read-modify-write it.
  manager "qconf -sc > /tmp/sc.cfg
    grep -qE '^${GPU_COMPLEX}[[:space:]]' /tmp/sc.cfg || \
      echo '${GPU_COMPLEX} ${GPU_COMPLEX} RSMAP <= YES YES 0 0' >> /tmp/sc.cfg
    qconf -Mc /tmp/sc.cfg >/dev/null"
  local ids w
  ids="$(seq -s ' ' 0 $((GPU_PER_WORKER - 1)))"
  for w in "${NODES[@]}"; do
    [ "$w" = "$MASTER" ] && continue
    manager "qconf -mattr exechost complex_values '${GPU_COMPLEX}=${GPU_PER_WORKER}(${ids})' $w"
  done
}
