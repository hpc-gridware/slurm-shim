#!/usr/bin/env bash
# Check: srun rejects an impossible request cleanly BEFORE launch (REQ-RUN-008),
# with a diagnostic and a non-zero exit -- not a hang. Guards the pre-flight path.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "70_reject: srun over-request fails fast with a diagnostic"

job="$(mktemp)"
trap 'rm -f "$job"' EXIT
cat >"$job" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=1
#SBATCH --ntasks-per-node=1
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh
# 1-slot allocation; ask for far more tasks than permitted.
srun -n 999 hostname 2>&1
echo "rc=$?"
EOF

remote=/home/gridware/e2e-70-reject.sh
out=/home/gridware/e2e-70-reject.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out")"
res="$(jobout "$id" "$out")"

assert_contains "$res" "srun:" "srun emitted a diagnostic for the rejected request"
assert_contains "$res" "rc=1" "srun exits 1 (clean pre-launch rejection, no hang)"
# The step must NOT have launched: no real hostname line should appear.
if printf '%s\n' "$res" | grep -qE '^ocs-(master|worker)'; then
  fail "a rank launched despite the over-request"
else
  pass "no rank launched (rejected before qrsh)"
fi
finish
