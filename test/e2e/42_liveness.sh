#!/usr/bin/env bash
# Check: a partitioned slave host is detected, and does not hang srun.
#
# Before liveness, a stepper on a host that vanished without closing its TCP
# connection (partition, power loss, frozen node) hung srun forever. The control
# channel now has kernel liveness (TCP keepalive + TCP_USER_TIMEOUT): when the
# peer HOST stops answering, the channel fails. srun then declares the node lost
# after ping_deadline and fails its tasks; the stale stepper on the partitioned
# host loses its channel after orphan_grace (shorter) and stops its ranks,
# exiting with a code, so the job is never deleted (REQ-CHN-004 as amended).
#
# Docker backend only: the partition is `docker network disconnect`.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "42_liveness: a partitioned slave is declared lost; srun and the job survive"

if [ "$BACKEND" != docker ]; then
  skip "partitions a node with docker network disconnect (backend is $BACKEND)"
  finish
fi
# Three nodes: the partitioned host must be a slave that is not the qmaster host.
# Cutting off the qmaster host is a control-plane outage, not the case under
# test; with three nodes at least one slave is always a plain exec host.
if [ "${EXEC_NODES:-0}" -lt 3 ]; then
  skip "needs 3 exec nodes, so a slave other than the qmaster host exists (cluster has ${EXEC_NODES:-0})"
  finish
fi

target="" id="" job=""
cleanup() {
  heal
  [ -n "$id" ] && gridware "qdel $id" >/dev/null 2>&1 || true
  rm -f "$job"
}
trap cleanup EXIT

# Short timers for the test, via the job's own config copy:
# the stepper gives up after 10s, srun after 15s.
cfg=$JOB_HOME/e2e-42-config.yaml
fast_liveness_config "$cfg"

job="$(mktemp)"
cat >"$job" <<EOF
#!/bin/bash
export SLURM_SHIM_CONFIG=$cfg
t0=\$SECONDS
srun bash -c 'hostname > "\$HOME/e2e-42.host.\$SLURM_PROCID"; exec sleep 342'
echo "SRUN_RC=\$? SECS=\$((SECONDS - t0))"
sleep 20
echo SCRIPT_END
EOF
remote=$JOB_HOME/e2e-42-liveness.sh
out=$JOB_HOME/e2e-42-liveness.out
put_job "$job" "$remote"
gridware "rm -f $JOB_HOME/e2e-42.host.*"
id="$(sbatch_submit "$remote" "$out" --nodes=3 --ntasks-per-node=1)"
if [ -z "$id" ]; then
  fail "sbatch returned no job id"
  finish
fi

# Wait for all ranks. Rank 0 runs on the job's master (srun's host); ranks 1 and
# 2 on the slaves. Partition a slave that is not the qmaster host.
for _ in $(seq 1 60); do
  [ "$(gridware "ls $JOB_HOME/e2e-42.host.* 2>/dev/null | wc -l" || true)" -ge 3 ] && break
  sleep 2
done
for r in 1 2; do
  h="$(gridware "cat $JOB_HOME/e2e-42.host.$r 2>/dev/null" || true)"
  if [ -n "$h" ] && [ "$h" != "$MASTER" ]; then
    target="$h"
    break
  fi
done
if [ -z "$target" ]; then
  fail "the step's ranks never started on a slave other than $MASTER"
  finish
fi
partition "$target"

# srun must notice and return while the host is still partitioned.
for _ in $(seq 1 45); do
  gridware "grep -q SRUN_RC= '$out'" 2>/dev/null && break
  sleep 2
done
# The stale stepper on the partitioned host lost its channel too (orphan_grace)
# and stopped its rank. docker exec still reaches a partitioned container.
rank_left=running
for _ in $(seq 1 15); do
  rank_left="$(node_sh "$target" "pgrep -f 'sleep 342' >/dev/null && echo running || echo gone" || true)"
  [ "$rank_left" = gone ] && break
  sleep 2
done
assert_eq "$rank_left" "gone" "the stale stepper on the partitioned host stopped its rank"
part_host=$target
heal

res="$(jobout "$id" "$out")"
assert_contains "$res" "lost node $part_host" "srun declares the partitioned node lost"
rc="$(printf '%s\n' "$res" | sed -n 's/.*SRUN_RC=\([0-9]*\).*/\1/p')"
secs="$(printf '%s\n' "$res" | sed -n 's/.*SECS=\([0-9]*\).*/\1/p')"
if [ -n "$rc" ] && [ "$rc" != 0 ]; then
  pass "srun returns non-zero ($rc): the lost node's tasks count as failed"
else
  fail "srun did not return a failure (SRUN_RC=${rc:-none})"
fi
if [ -n "$secs" ] && [ "$secs" -lt 75 ]; then
  pass "srun returned in ${secs}s instead of hanging"
else
  fail "srun did not return in time (SECS=${secs:-none})"
fi
assert_contains "$res" "SCRIPT_END" "the batch script runs to its end"
assert_eq "$(failed_field "$id")" "0" "qacct failed is 0, not 100: no pe task died by signal"
id=""

# Leave the host healthy for the next check.
wait_host_up "$part_host"

finish
