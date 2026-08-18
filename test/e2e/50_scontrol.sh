#!/usr/bin/env bash
# Check: scontrol show hostnames expands a compressed SLURM nodelist. This is the
# call many launchers (accelerate, torchrun wrappers) use to build a host list.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "50_scontrol: show hostnames expands a compressed nodelist"

# Pure function of its argument -- no job needed.
out="$(gridware "scontrol show hostnames 'ocs-worker[1-2],ocs-master'")"
n="$(printf '%s\n' "$out" | grep -c .)"
assert_eq "$n" "3" "expands to 3 hostnames"
assert_contains "$out" "ocs-worker1" "includes ocs-worker1"
assert_contains "$out" "ocs-worker2" "includes ocs-worker2"
assert_contains "$out" "ocs-master" "includes ocs-master"

# One-per-line output (the contract launchers rely on).
first="$(printf '%s\n' "$out" | head -1)"
case "$first" in
  *" "*|*",") fail "hostnames are not one-per-line: '$first'" ;;
  *) pass "hostnames printed one per line" ;;
esac

# scontrol show job <id>: submit a sleeper WITHOUT -p (exercises default_partition)
# then query it from this login shell (GE-backed, no in-job layout present).
id="$(gridware 'sbatch --wrap="sleep 60"' | awk '/Submitted batch job/{print $NF}')"
if [ -n "$id" ]; then pass "sbatch without -p used the default partition (job $id)"; else fail "sbatch without -p did not submit"; fi
# Wait until the job is actually RUNNING (not merely present/pending) so the
# JobState=RUNNING assertion below is not racy.
for _ in $(seq 1 20); do
  [ "$(gridware "squeue -h -o %t -j '$id' 2>/dev/null | tr -d ' '")" = "R" ] && break
  sleep 2
done
job="$(gridware "scontrol show job '$id'")"
assert_contains "$job" "JobId=$id" "scontrol show job reports the job id"
assert_contains "$job" "JobState=RUNNING" "scontrol show job maps the GE state to RUNNING"
unknown="$(gridware 'scontrol show job 999999 2>&1 || true')"
assert_contains "$unknown" "Invalid job id" "scontrol show job rejects an unknown id cleanly"
gridware "scancel '$id' >/dev/null 2>&1; qdel '$id' >/dev/null 2>&1 || true"
finish
