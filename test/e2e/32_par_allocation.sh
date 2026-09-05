#!/usr/bin/env bash
# Check: --nodes / --ntasks-per-node actually pin the layout, via `qsub -par`.
#
# Why this exists separately from the unit tests: internal/cli/sbatch asserts the
# generated argv against a FAKE runner. That proves we emit `-par 2 -w e`; it
# proves nothing about whether Grid Engine honors the rule, whether the resulting
# PE_HOSTFILE is uniform, or whether `-w e` really refuses an impossible layout
# instead of accepting it and leaving the job in qw forever. Every assertion here
# reads back from a real qsub.
#
# Gated on the OCS the cluster is actually running: -par landed in 9.1.5, and the
# suite must stay green on the 9.0.10 leg of the matrix, where the shim degrades
# to letting the PE place the nodes.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "32_par_allocation: --nodes/--ntasks-per-node pin the layout via qsub -par"

CLEANUP_IDS=""
cleanup() { [ -n "$CLEANUP_IDS" ] && gridware "qdel $CLEANUP_IDS >/dev/null 2>&1" || true; }
trap cleanup EXIT

submit() { gridware "cd && sbatch $*" 2>/dev/null | awk '/Submitted batch job/{print $NF}' || true; }
jattr() {
  gridware "qstat -j '$1' 2>/dev/null | awk -F: '/^$2/{sub(/^[^:]*:/,\"\"); print; exit}'" \
    | tr -d '[:space:]'
}

ocs="$(ocs_version)"
if ! version_ge "${ocs:-0}" 9.1.5; then
  skip "qsub -par needs OCS 9.1.5 (cluster runs ${ocs:-unknown})"
  finish
fi

# A job that outlives the assertions, so qstat -j still has it. It sleeps rather
# than exiting because hard_resource_list and allocation_rule are only readable
# while the job is live.
SLEEPER='--wrap="sleep 45"'

# --------------------------------------------- a multi-node layout is pinned
# The batch partition's PE is $round_robin, so before -par this exact request
# landed 2 slots on each of 3 hosts only because the cluster happens to have 3.
id="$(submit "-p batch -N 3 --ntasks-per-node=2 $SLEEPER")"
if [ -z "$id" ]; then
  fail "sbatch -N 3 --ntasks-per-node=2 was refused"
else
  CLEANUP_IDS="$CLEANUP_IDS $id"
  assert_eq "$(jattr "$id" allocation_rule)" "2" "-N 3 --ntasks-per-node=2 pins 2 slots per node"
  assert_eq "$(jattr "$id" parallel_environment)" "makerange:6" "the slot count is unchanged by -par"
fi

# ------------------------------------------- a single-node layout is pinned too
# On the SAME round-robin PE: this is the placement that used to need a separate
# $pe_slots partition.
id1="$(submit "-p batch -N 1 -c 4 $SLEEPER")"
if [ -z "$id1" ]; then
  fail "sbatch -N 1 -c 4 was refused"
else
  CLEANUP_IDS="$CLEANUP_IDS $id1"
  assert_eq "$(jattr "$id1" allocation_rule)" '$pe_slots' "-N 1 pins the job to one node"
fi

# ------------------------------------------------- an uneven layout still runs
# Grid Engine cannot grant 3,2,2, so nothing is pinned -- but the job must still
# submit, with a warning rather than a refusal.
warn="$(gridware "cd && sbatch -p batch -N 3 -n 7 $SLEEPER" 2>&1 || true)"
assert_contains "$warn" "uneven" "an inexpressible layout warns"
assert_contains "$warn" "Submitted batch job" "and the job is still submitted"
id2="$(printf '%s' "$warn" | awk '/Submitted batch job/{print $NF}')"
[ -n "$id2" ] && CLEANUP_IDS="$CLEANUP_IDS $id2"
if [ -n "$id2" ]; then
  assert_eq "$(jattr "$id2" allocation_rule)" "" "no allocation rule is pinned for it"
fi

# ------------------------------------- an impossible layout is refused at submit
# The regression this guards: without -w e, qsub ACCEPTS this and the job sits in
# qw forever. Assert the refusal, never that a qw job eventually clears.
# Compare job IDs, not a count: a count also moves when an unrelated job from an
# earlier check finishes mid-window, which made this fail spuriously (6 -> 5) with
# nothing wrong. Only a NEW id means the refused job was queued.
jobids() { gridware "qstat -u gridware 2>/dev/null | tail -n +3 | awk '{print \$1}'" | sort; }
before_ids="$(jobids)"
out="$(gridware "cd && sbatch -p batch -N 99 $SLEEPER" 2>&1 || true)"
assert_contains "$out" "Requested node configuration is not available" \
  "a layout no host set can satisfy is refused at submit"
case "$out" in
  *"Submitted batch job"*) fail "nothing must be submitted for a refused layout" ;;
  *) pass "and nothing is submitted" ;;
esac
new_ids="$(comm -13 <(printf '%s\n' "$before_ids") <(jobids) | grep -c . || true)"
assert_eq "$new_ids" "0" "the refused job was never queued"

finish
