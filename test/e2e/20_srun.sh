#!/usr/bin/env bash
# Check: srun fans ranks across the allocation with correct per-rank identity and
# -l line labels, over qrsh -inherit tight integration.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "20_srun: srun -n fan-out + per-rank env + -l labels"

job="$(mktemp)"
trap 'rm -f "$job"' EXIT
cat >"$job" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=2
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh
srun -l -n 6 bash -c 'echo "rank=$SLURM_PROCID node=$SLURM_NODEID host=$(hostname)"'
EOF

remote=/home/gridware/e2e-20-srun.sh
out=/home/gridware/e2e-20-srun.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out")"
res="$(jobout "$id" "$out")"

n="$(printf '%s\n' "$res" | grep -c 'rank=')"
assert_eq "$n" "6" "srun launched 6 ranks (one line each)"
# -l prefixes each stdout line with "<rank>: "; ranks 0 and 5 must both appear.
assert_contains "$res" "0: rank=0" "rank 0 present and -l labelled"
assert_contains "$res" "5: rank=5" "rank 5 present and -l labelled"
# Ranks land on more than one host (real multi-node fan-out, not all local).
hosts="$(printf '%s\n' "$res" | grep -oE 'host=[^ ]+' | sort -u | wc -l | tr -d ' ')"
if [ "$hosts" -gt 1 ]; then pass "ranks spread across $hosts hosts"; else fail "ranks did not spread (hosts=$hosts)"; fi
finish
