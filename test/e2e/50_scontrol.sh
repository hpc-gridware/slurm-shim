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
finish
