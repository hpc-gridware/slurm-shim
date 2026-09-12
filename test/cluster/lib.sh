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
# OCS_VERSION defaults to 9.1.5 so the harness is deterministic (not "whatever
# quickinstall's latest happens to be"); override e.g. OCS_VERSION=9.0.10.
QUICKINSTALL_REPO="${QUICKINSTALL_REPO:-https://github.com/hpc-gridware/quickinstall.git}"
QUICKINSTALL_REF="${QUICKINSTALL_REF:-main}"
QUICKINSTALL_DIR="${QUICKINSTALL_DIR:-}"   # use an existing checkout instead of cloning
OCS_VERSION="${OCS_VERSION:-9.1.5}"        # default OCS package version
READY_TIMEOUT="${READY_TIMEOUT:-360}"

SHIM_PREFIX="${SHIM_PREFIX:-/opt/slurm-shim}"   # identical absolute path on every node
CELL_DIR="${CELL_DIR:-/opt/ocs/default/common}" # the cell; config.yaml lives in $CELL_DIR/slurm-shim (per-node $SGE_ROOT here)
COMPOSE_SUBDIR=containers/openSUSE/15.6

# BACKEND selects how the harness reaches the cluster. `docker` (default) is the
# local quickinstall container cluster; `ssh` is any real cluster -- the GPU
# validation cluster included -- so the e2e suite runs unchanged against both
# instead of being forked. A remote cluster overrides NODES/MASTER with real
# hostnames: NODES="mgr g1 g2 g3 g4" MASTER=mgr BACKEND=ssh make e2e
BACKEND="${BACKEND:-docker}"
# SSH_CMD is invoked as: $SSH_CMD [user@]<host> "<remote command>", so a wrapper
# must accept the host as its first positional argument. Plain `ssh` is the
# right answer: put the IAP ProxyCommand and ControlMaster in ~/.ssh/config
# instead. `gcloud compute ssh --tunnel-through-iap --` does NOT work here (it
# wants the instance BEFORE `--`), and it rebuilds the tunnel on every call,
# which is ruinous for a suite that makes hundreds of them.
SSH_CMD="${SSH_CMD:-ssh}"
SCP_CMD="${SCP_CMD:-scp}"                 # must accept scp syntax, incl. -q
SSH_USER="${SSH_USER:-}"                  # empty: let ssh pick (config/agent)

# Env cannot carry a bash array, so NODES arrives as a space-separated string.
# NODES_SPEC holds that scalar and is EXPORTED, which is load-bearing twice
# over: re-sourcing this file would otherwise re-read "${NODES:-...}", see the
# ARRAY, and expand only its first element; and `run.sh` runs every check as a
# SEPARATE process, where the array does not survive at all (bash cannot export
# arrays) -- so without the exported scalar each check would silently fall back
# to the container node list and test the wrong cluster.
: "${NODES_SPEC:=${NODES:-ocs-master ocs-worker1 ocs-worker2}}"
read -r -a NODES <<< "$NODES_SPEC"
MASTER="${MASTER:-ocs-master}"
JOB_USER="${JOB_USER:-gridware}"          # unprivileged user that submits jobs
# Where that user's home lives. NOT always /home/$JOB_USER: on a cloud cluster
# /home belongs to the SSH login user the provider's guest agent manages, and
# NFS-mounting a shared /home over it shadows that user's authorized_keys and
# locks you out of every node at once. Cluster homes therefore live on their own
# export, and this names it.
JOB_HOME="${JOB_HOME:-/home/$JOB_USER}"
# Everything a child check process needs to reach the same cluster.
export NODES_SPEC MASTER JOB_USER JOB_HOME BACKEND SSH_CMD SCP_CMD SSH_USER \
       SHIM_PREFIX CELL_DIR

# Readiness signal: how many instances of which queue must exist. Defaults to one
# per node, which is what the container cluster has; a cluster whose manager runs
# no execd sets READY_INSTANCES to the exec-host count.
READY_QUEUE="${READY_QUEUE:-all.q}"
READY_INSTANCES="${READY_INSTANCES:-${#NODES[@]}}"

GPU_COMPLEX="${GPU_COMPLEX:-gpu}"
GPU_PER_WORKER="${GPU_PER_WORKER:-2}"     # fake RSMAP device count per worker
# Consumable scope of the RSMAP complex. The container cluster fakes per-slot
# YES; real hardware wants HOST, because the shim's `-l gpu=N` means "per node"
# and a per-slot complex multiplies it by the node's slot count.
GPU_CONSUMABLE="${GPU_CONSUMABLE:-YES}"
FLAX_VENV="${FLAX_VENV:-/home/gridware/flaxenv}"   # shared venv for the flax demo
# Unpinned on purpose (the recipe tracks current jax/flax), but overridable the
# same way OCS_VERSION is, so a breaking release can be worked around without
# editing this file: FLAX_PIP_SPEC='jax==0.11.1 flax==0.12.9 optax' make demo-flax
FLAX_PIP_SPEC="${FLAX_PIP_SPEC:-jax flax optax}"

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
# through (defaults to 9.1.5; see knobs).
compose() {
  ( cd "$(qi_dir)" && OCS_VERSION="$OCS_VERSION" docker compose "$@" )
}

# --- transport ---------------------------------------------------------------
# Three primitives carry every cluster interaction: run a command as root, run it
# as another user, and copy a file in. Each backend implements those three and
# nothing else, so a new transport cannot half-work.
#
# The command travels on STDIN rather than as an argument, so it survives both
# backends verbatim -- no shell, ssh, or sudo quoting layer ever re-parses it.
# Nothing in the harness pipes data into these helpers, which is what makes
# stdin free to use.

# node_sh <node> <command...> runs a command as root in a login shell.
node_sh() {
  local node="$1"; shift
  case "$BACKEND" in
    docker) printf '%s' "$*" | docker exec -i "$node" bash -l -s ;;
    ssh)    printf '%s' "$*" | $SSH_CMD "${SSH_USER:+$SSH_USER@}$node" "sudo bash -l -s" ;;
    *)      die "unknown BACKEND '$BACKEND' (want: docker, ssh)" ;;
  esac
}

# node_sh_as <user> <node> <command...> runs it as an unprivileged user.
node_sh_as() {
  local user="$1" node="$2"; shift 2
  case "$BACKEND" in
    docker) printf '%s' "$*" | docker exec -i -u "$user" "$node" bash -l -s ;;
    # -H sets HOME to the target user's. Without it sudo keeps the LOGIN
    # user's HOME, so `gridware`'s bare `cd` lands in a directory the job user
    # cannot write -- breaking -cwd submit-dir semantics and relative --output.
    ssh)    printf '%s' "$*" | $SSH_CMD "${SSH_USER:+$SSH_USER@}$node" "sudo -H -u $user bash -l -s" ;;
    *)      die "unknown BACKEND '$BACKEND' (want: docker, ssh)" ;;
  esac
}

# node_put <local-file> <node> <remote-path> copies a file in as root. Over ssh
# the login user usually cannot write the destination, so stage in /tmp and move
# it into place with the root primitive.
node_put() {
  local src="$1" node="$2" dst="$3"
  case "$BACKEND" in
    docker) docker cp "$src" "$node:$dst" >/dev/null ;;
    ssh)
      local stage
      stage="/tmp/.harness.$$.$(basename "$dst")"
      $SCP_CMD -q "$src" "${SSH_USER:+$SSH_USER@}$node:$stage" >/dev/null \
        || die "could not copy $src to $node:$stage"
      node_sh "$node" "mv -f '$stage' '$dst'"
      ;;
    *) die "unknown BACKEND '$BACKEND' (want: docker, ssh)" ;;
  esac
}

# node_running <node> succeeds when the node is up and reachable.
node_running() {
  case "$BACKEND" in
    docker) docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null | grep -qx true ;;
    ssh)    node_sh "$1" true >/dev/null 2>&1 ;;
    *)      die "unknown BACKEND '$BACKEND' (want: docker, ssh)" ;;
  esac
}

require_cluster() {
  for n in "${NODES[@]}"; do
    node_running "$n" || die "node $n is not reachable (BACKEND=$BACKEND; for docker run: make cluster-up)"
  done
  EXEC_NODES="$(exec_node_count)"
  export EXEC_NODES
}

# exec_node_count: how many hosts can actually RUN a job.
#
# Not `qconf -sel`: that lists every admin/exec host, including a manager that
# carries no queue instance -- so a job asking for that many nodes is refused
# with "Requested node configuration is not available", which reads like a
# broken cluster rather than a miscount. The queue hostlist is the set that can
# actually take work.
#
# A hostlist may name an @hostgroup, and qconf WRAPS long values with a
# backslash continuation, so unfold before splitting.
exec_node_count() {
  local n
  n="$(manager "
    hl=\$(qconf -sq ${READY_QUEUE:-all.q} 2>/dev/null \
          | sed -e :a -e '/\\\\\$/N; s/\\\\\\n[[:space:]]*/ /; ta' \
          | awk '/^hostlist/ { \$1=\"\"; print }')
    for h in \$hl; do
      case \"\$h\" in
        @*) qconf -shgrp \"\$h\" 2>/dev/null | awk '/^hostlist/ { \$1=\"\"; print }' ;;
        *)  echo \"\$h\" ;;
      esac
    done | tr ' ' '\n' | sed '/^\$/d' | sort -u | grep -c . || true" 2>/dev/null | tr -dc '0-9')"
  [ -n "$n" ] && [ "$n" -gt 0 ] && echo "$n" || echo 1
}

# wait_ready blocks until every node has a queue instance (per the quickinstall
# README: qhost alone is not enough, qstat -f is the real signal).
wait_ready() {
  log "waiting for the cluster ($READY_INSTANCES $READY_QUEUE instances)"
  local deadline=$((SECONDS + READY_TIMEOUT)) n
  while :; do
    # grep -c prints 0 AND exits 1 when nothing matches, so `|| echo 0` would
    # append a second 0. Normalise to the first line instead.
    n="$(node_sh "$MASTER" "qstat -f 2>/dev/null | grep -c '^${READY_QUEUE//./\\.}@' || true" 2>/dev/null | head -1)"
    [ "${n:-0}" = "$READY_INSTANCES" ] && { log "cluster ready"; return 0; }
    [ $SECONDS -lt $deadline ] || die "cluster not ready within ${READY_TIMEOUT}s (have $n/$READY_INSTANCES queue instances)"
    sleep 5
  done
}

# require_scheduler fails fast when the qmaster accepts jobs but nothing
# dispatches. A dead scheduler thread is not a hypothetical: it happens after the
# host suspends, and the symptom is jobs sitting in qw while every queue is idle.
# Without this probe the suite does not fail, it HANGS -- `srun --pty` maps to
# `qrsh -now no`, which waits forever for a dispatch that never comes. On billed
# hardware that is an expensive way to learn the cluster is sick.
SCHED_PROBE_TIMEOUT="${SCHED_PROBE_TIMEOUT:-60}"
require_scheduler() {
  local id deadline state
  # `|| true`: under `set -e` + `pipefail` a dead qmaster fails the pipeline and
  # would kill the suite here, before the diagnostic below could ever print.
  id="$(gridware "qsub -terse -b y -o /dev/null -j y /bin/true 2>/dev/null" | tr -dc '0-9' || true)"
  [ -n "$id" ] || die "could not submit the scheduler probe job (is the qmaster up?)"
  deadline=$((SECONDS + SCHED_PROBE_TIMEOUT))
  while :; do
    # Gone from qstat means it ran and left: the scheduler is dispatching.
    state="$(gridware "qstat -j $id >/dev/null 2>&1 && echo present || echo gone")"
    [ "$state" = gone ] && return 0
    if [ $SECONDS -ge $deadline ]; then
      gridware "qdel $id >/dev/null 2>&1" || true
      die "probe job $id never dispatched in ${SCHED_PROBE_TIMEOUT}s while the cluster looks idle.
  The scheduler thread is probably dead (classic after a host suspend). Restart it:
    qconf -kt scheduler && qconf -at scheduler
  Then re-run. Override the wait with SCHED_PROBE_TIMEOUT."
    fi
    sleep 2
  done
}

# ocs_version prints the version the cluster is actually RUNNING, e.g. "9.1.5".
# Not the same thing as $OCS_VERSION, which is only what was asked for: a cluster
# left up from an earlier run answers with what it has installed. Checks that
# depend on a version-specific fix must ask the cluster, not the knob.
ocs_version() {
  node_sh "$MASTER" 'qstat -help 2>&1 | head -1' 2>/dev/null \
    | awk '{ for (i=1;i<=NF;i++) if ($i ~ /^[0-9]+\.[0-9]+\.[0-9]+$/) { print $i; exit } }'
}

# container_arch maps the master's uname -m to a GOARCH.
container_arch() {
  case "$(node_sh "$MASTER" 'uname -m' 2>/dev/null | tr -d '[:space:]')" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64)  echo amd64 ;;
    *) die "unsupported node arch" ;;
  esac
}

# gridware runs a login shell command as the job user on the master (OCS env
# sourced via /etc/profile.d). Login shell is non-interactive so no banner prints.
# cd first: the transport starts in an unspecified directory, and with the shim's
# SLURM-parity default (jobs run in the submit dir, -cwd) submitting from an
# unwritable cwd would fail output-file creation -- as it would on real SLURM.
gridware() { node_sh_as "$JOB_USER" "$MASTER" "cd && $*"; }

# manager runs a command as root on the master. Root is a GE manager on the
# container cluster, so qconf mutations (PE hooks, complexes) go through it.
manager() { node_sh "$MASTER" "$*"; }

# queue_host prints a host that actually has an instance of $READY_QUEUE, and
# gpu_host one that declares the GPU complex. Checks that must pin a job to a
# specific queue instance use these instead of naming a container: on a real
# cluster the manager often runs no execd at all, so "the master" is not a queue
# instance and hardcoding one makes the check unrunnable off the container tree.
# Returns the SHORT name, which is what a job reports in SLURM_JOB_NODELIST and
# what `hostname -s` gives. Deliberately not parsed from `qstat -f`: that column
# is TRUNCATED (a 30-char cut of the FQDN), so it yields a host that does not
# exist and comparisons against job output silently fail.
queue_host() {
  manager "qconf -sel 2>/dev/null | head -1 | cut -d. -f1" | tr -d '[:space:]'
}

# Prefers a host that is NOT the manager: on a real cluster the manager runs no
# execd and has no devices, and on the container cluster this keeps picking the
# same worker the checks have always used.
gpu_host() {
  local h
  h="$(manager "for h in \$(qconf -sel); do
      [ \"\$h\" = '$MASTER' ] && continue
      if qconf -se \$h | tr -d ' ' | grep -q '${GPU_COMPLEX}=[0-9]'; then echo \$h | cut -d. -f1; break; fi
    done" | tr -d '[:space:]')"
  [ -n "$h" ] || h="$(manager "for h in \$(qconf -sel); do
      if qconf -se \$h | tr -d ' ' | grep -q '${GPU_COMPLEX}=[0-9]'; then echo \$h | cut -d. -f1; break; fi
    done" | tr -d '[:space:]')"
  printf '%s' "$h"
}

# ensure_gpu_complex configures a fake RSMAP GPU complex so the GPU path can be
# exercised without real hardware. Idempotent: adds the complex if absent and sets
# GPU_PER_WORKER devices on every non-master node. Shared by install-shim.sh
# (--gpu) and the e2e gpu check.
ensure_gpu_complex() {
  log "ensuring fake RSMAP complex '$GPU_COMPLEX' ($GPU_PER_WORKER/worker)"
  # qconf -Mc replaces the whole complex list, so read-modify-write it.
  # Rewrite the consumable column rather than only appending when absent: an
  # append-only guard makes GPU_CONSUMABLE a silent no-op on any cluster that
  # already defines the complex -- which is every re-run, and every real site.
  # Getting the scope wrong is not a visible error either: `-l gpu=N` against a
  # per-slot complex is multiplied by the node's slot count and the job simply
  # never dispatches. Field 6 is `consumable` in the 8-column complex format.
  manager "qconf -sc > /tmp/sc.cfg
    awk -v c='${GPU_COMPLEX}' -v cons='${GPU_CONSUMABLE}' '
      \$1 == c { \$6 = cons; found = 1 }
      { print }
      END { if (!found) print c, c, \"RSMAP\", \"<=\", \"YES\", cons, 0, 0 }
    ' /tmp/sc.cfg > /tmp/sc.new
    qconf -Mc /tmp/sc.new >/dev/null"
  local ids w
  ids="$(seq -s ' ' 0 $((GPU_PER_WORKER - 1)))"
  for w in "${NODES[@]}"; do
    [ "$w" = "$MASTER" ] && continue
    manager "qconf -mattr exechost complex_values '${GPU_COMPLEX}=${GPU_PER_WORKER}(${ids})' $w"
  done
}

# ensure_flax_env makes jax/flax/optax importable on every node for the flax demo.
# The containers ship no usable Python, so this installs Python 3.11 on each node
# and creates ONE venv on the shared home, which every node then sees at the same
# path (the same shape the ray recipe documents). Idempotent; the first run
# downloads ~200 MB and needs network from the containers.
ensure_flax_env() {
  local n
  for n in "${NODES[@]}"; do
    if node_sh "$n" 'command -v python3.11 >/dev/null 2>&1'; then continue; fi
    log "installing Python 3.11 on $n"
    node_sh "$n" 'zypper -n --gpg-auto-import-keys install python311 python311-pip >/dev/null' \
      || die "could not install python311 on $n"
  done
  if ! node_sh "$MASTER" "test -x '$FLAX_VENV/bin/python'"; then
    log "creating shared venv $FLAX_VENV (jax, flax, optax; downloads ~200 MB)"
    gridware "python3.11 -m venv '$FLAX_VENV'" || die "could not create $FLAX_VENV"
    gridware "'$FLAX_VENV/bin/pip' install --quiet --upgrade pip" >/dev/null 2>&1 || true
    gridware "'$FLAX_VENV/bin/pip' install --quiet $FLAX_PIP_SPEC" || die "pip install $FLAX_PIP_SPEC failed"
  fi
  # The venv's python is a symlink to the system 3.11, so a node that skipped the
  # install above would fail here rather than inside the job.
  gridware "'$FLAX_VENV/bin/python' -c 'import jax, flax, optax; print(\"jax\", jax.__version__, \"flax\", flax.__version__, \"optax\", optax.__version__)'" \
    || die "$FLAX_VENV is not importable"
}
