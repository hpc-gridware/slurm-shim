#!/usr/bin/env bash
# Check: sinfo reports live node counts and states from GE (not placeholders).
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "80_sinfo: partition table with live node counts/states"

out="$(gridware 'sinfo')"
assert_contains "$out" "PARTITION AVAIL TIMELIMIT NODES STATE NODELIST" "prints the SLURM header"

# Live query worked -> no 'n/a' placeholder rows.
if printf '%s\n' "$out" | grep -q ' n/a '; then
  fail "sinfo fell back to placeholders (n/a) -- GE query did not populate"
else
  pass "no placeholder rows (live node states populated)"
fi

# batch and gpu both map to all.q on every queue host; the NODES column across a
# partition's state rows must total the cluster's size (robust to a node being
# split off by state). The count is DERIVED: hardcoding 3 only worked on the
# container harness and fails on any other cluster shape.
for part in batch gpu; do
  total="$(printf '%s\n' "$out" | awk -v p="$part" '$1==p {n+=$4} END{print n+0}')"
  assert_eq "$total" "$EXEC_NODES" "$part totals $EXEC_NODES node(s) across state rows"
done
finish
