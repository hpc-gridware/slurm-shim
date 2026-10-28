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
#SBATCH --nodes=@EXEC_NODES@
#SBATCH --ntasks-per-node=2
srun -l -n 6 bash -c 'echo "rank=$SLURM_PROCID node=$SLURM_NODEID host=$(hostname)"'
# -n below the step's capacity: one task per -N node, and no node without one.
if [ "$SLURM_NNODES" -ge 2 ]; then
  srun -N 2 -n 2 bash -c 'echo "spread host=$(hostname) tpn=$SLURM_STEP_TASKS_PER_NODE"'
fi
srun -n 1 bash -c 'echo "single nodes=$SLURM_STEP_NUM_NODES"'
EOF
sed -i.bak "s|@EXEC_NODES@|${EXEC_NODES:-1}|" "$job" && rm -f "$job.bak"

remote=$JOB_HOME/e2e-20-srun.sh
out=$JOB_HOME/e2e-20-srun.out
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

if [ "${EXEC_NODES:-1}" -ge 2 ]; then
  spread="$(printf '%s\n' "$res" | grep -oE 'spread host=[^ ]+' | sort -u | wc -l | tr -d ' ')"
  assert_eq "$spread" "2" "srun -N 2 -n 2 runs one task on each of 2 hosts"
  assert_contains "$res" "tpn=1(x2)" "srun -N 2 -n 2 step tasks per node is 1(x2)"
fi
assert_contains "$res" "single nodes=1" "srun -n 1 is a one-node step"
finish
