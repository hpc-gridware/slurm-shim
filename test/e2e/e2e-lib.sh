#!/usr/bin/env bash
# Shared helpers and assertions for the e2e checks. Each NN_*.sh sources this,
# runs its assertions, and ends with `finish` (exit non-zero if any failed).
#
# The cluster helpers (gridware/manager/require_cluster/MASTER/NODES/...) come
# from the Phase 1 cluster lib, reused verbatim so there is one source of truth.
set -uo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../cluster/lib.sh
source "$E2E_DIR/../cluster/lib.sh"

E2E_PASS=0
E2E_FAIL=0

pass() { E2E_PASS=$((E2E_PASS + 1)); printf '  \033[1;32mok\033[0m   %s\n' "$*"; }
fail() { E2E_FAIL=$((E2E_FAIL + 1)); printf '  \033[1;31mFAIL\033[0m %s\n' "$*" >&2; }
skip() { printf '  \033[1;33mskip\033[0m %s\n' "$*"; }

# assert_contains <haystack> <needle> <label>
assert_contains() {
  case "$1" in
    *"$2"*) pass "$3" ;;
    *) fail "$3 -- missing '$2' in: $(printf '%s' "$1" | tr '\n' '|')" ;;
  esac
}

# assert_eq <got> <want> <label>
assert_eq() {
  if [ "$1" = "$2" ]; then pass "$3"; else fail "$3 -- want '$2' got '$1'"; fi
}

# version_ge <a> <b> succeeds when dotted-numeric version a >= b. Used to gate a
# check on an OCS fix that landed in a known release, so the same suite stays
# green across the version matrix instead of asserting the newest behavior
# everywhere. Hand-rolled because macOS sort has no -V.
version_ge() {
  awk -v a="$1" -v b="$2" 'BEGIN {
    na = split(a, x, "."); nb = split(b, y, ".")
    n = (na > nb) ? na : nb
    for (i = 1; i <= n; i++) {
      p = (i <= na) ? x[i] + 0 : 0
      q = (i <= nb) ? y[i] + 0 : 0
      if (p > q) exit 0
      if (p < q) exit 1
    }
    exit 0
  }'
}

# finish exits with 1 if any assertion in this check failed.
finish() {
  if [ "$E2E_FAIL" -gt 0 ]; then
    printf '  -> %d passed, \033[1;31m%d failed\033[0m\n' "$E2E_PASS" "$E2E_FAIL" >&2
    exit 1
  fi
  printf '  -> %d passed\n' "$E2E_PASS"
  exit 0
}

# put_job <local-script> <remote-path> copies a job script to the master owned by
# the job user.
put_job() {
  node_put "$1" "$MASTER" "$2"
  # Owner only, no group: on a real cluster the submit user's primary group is
  # commonly `users` or a directory group, not one named after the user.
  node_sh "$MASTER" "chown $JOB_USER '$2'"
}

# jobout <jobid> <outfile> waits (bounded) for the job to leave the queue, then
# prints the output file. Empty if it never produced one.
#
# The wait is JOB_DEADLINE seconds (default 180). It used to be a hardcoded
# 90 x 2s, which quietly truncated every long check: a GPU training job given
# JOB_DEADLINE=1800 was read at 180s and reported as "no steps logged" -- a false
# failure, on the most expensive checks in the campaign.
#
# It also ALWAYS succeeds. Callers assign it in a command substitution, and the
# harness runs under `set -e`, so a missing output file (job not dispatched yet)
# used to abort the whole check before it could report anything -- the caller
# saw "produced NO SUMMARY", which reads as a harness bug rather than a slow
# job. "Empty if it never produced one" was the documented contract all along;
# this makes it true, so no call site needs its own `|| true`.
jobout() {
  local id="$1" out="$2"
  local deadline=$(( $(date +%s) + ${JOB_DEADLINE:-180} ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    gridware "squeue -h -j '$id' 2>/dev/null | grep -q ." || break
    sleep 2
  done
  gridware "cat '$out' 2>/dev/null" || true
}

# sbatch_submit <remote-script> <outfile> [extra sbatch args...] submits through
# the shim's sbatch and echoes the numeric job id (or empty on failure).
sbatch_submit() {
  local script="$1" out="$2"
  shift 2
  gridware "rm -f '$out'"
  gridware "sbatch $* --output='$out' '$script'" | awk '/Submitted batch job/{print $NF}'
}

# failed_field <jobid> prints the leading code of qacct's `failed` field once the
# job reaches accounting (it carries a ": description" suffix when non-zero).
# Empty if the job never shows up within 90s.
failed_field() {
  for _ in $(seq 1 45); do
    local f
    f="$(gridware "qacct -j '$1' 2>/dev/null | awk '/^failed/{print \$2; exit}'" || true)"
    [ -n "$f" ] && { echo "$f"; return 0; }
    sleep 2
  done
}

# partition <container> [log-suffix] cuts a node off the network (docker
# backend): it remembers the container's network and IP, then disconnects it.
# heal reconnects it with the same IP; it is a no-op when nothing is
# partitioned, so it is safe to call twice and from an EXIT trap.
PARTITIONED="" PARTITION_NET="" PARTITION_IP=""
partition() {
  PARTITIONED=$1
  read -r PARTITION_NET PARTITION_IP < <(docker inspect "$1" --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{$v.IPAddress}}{{end}}')
  log "partitioning $1 ($PARTITION_NET $PARTITION_IP)${2:-}"
  docker network disconnect "$PARTITION_NET" "$1"
}
heal() {
  [ -n "$PARTITIONED" ] && [ -n "$PARTITION_NET" ] &&
    docker network connect --ip "$PARTITION_IP" "$PARTITION_NET" "$PARTITIONED" >/dev/null 2>&1
  PARTITIONED=""
}

# fast_liveness_config <path> copies the cell's shim config to <path>, owned by
# the job user, with short liveness timers: a stale stepper gives up after 10s,
# srun declares a node lost after 15s. Jobs use it via SLURM_SHIM_CONFIG.
fast_liveness_config() {
  local cfg=$1 kv k v
  manager "cp $CELL_DIR/slurm-shim/config.yaml $cfg && chown $JOB_USER $cfg"
  for kv in ping_interval:2s orphan_grace:10s ping_deadline:15s; do
    k=${kv%%:*} v=${kv#*:}
    manager "grep -q '^$k:' $cfg && sed -i 's/^$k:.*/$k: $v/' $cfg || echo '$k: $v' >> $cfg"
  done
}

# require_spares_cluster skips the check (and finishes) unless the cluster can
# run a hot-spare test: docker backend (the lost node is partitioned), 3 exec
# nodes (2 nodes + 1 spare) and OCS 9.1.5+ (spares need qsub -par).
require_spares_cluster() {
  if [ "$BACKEND" != docker ]; then
    skip "partitions a node with docker network disconnect (backend is $BACKEND)"
    finish
  fi
  if [ "${EXEC_NODES:-0}" -lt 3 ]; then
    skip "needs 3 exec nodes for 2 nodes + 1 spare (cluster has ${EXEC_NODES:-0})"
    finish
  fi
  local ocs
  ocs="$(ocs_version)"
  if ! version_ge "${ocs:-0}" 9.1.5; then
    skip "hot spares need qsub -par, OCS 9.1.5+ (cluster runs ${ocs:-unknown})"
    finish
  fi
}

# wait_host_up <host> waits (bounded, 60s) until no queue instance on host is
# in state u, so a partitioned node is healthy again for the next check.
wait_host_up() {
  for _ in $(seq 1 30); do
    manager "qstat -f | grep '@$1 ' | grep -q ' u'" || break
    sleep 2
  done
}
