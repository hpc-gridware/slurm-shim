#!/usr/bin/env bash
# Check: squeue lists a live job (GE state -> SLURM state mapping) and scancel
# removes it.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "40_squeue_scancel: squeue lists a job, scancel removes it"

job="$(mktemp)"
trap 'rm -f "$job"' EXIT
cat >"$job" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=1
#SBATCH --ntasks-per-node=1
sleep 120
EOF

remote=/home/gridware/e2e-40-sleep.sh
out=/home/gridware/e2e-40-sleep.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out")"

# Wait until squeue reports the job at all (queued or running).
seen=""
for _ in $(seq 1 20); do
  if gridware "squeue -h -j '$id' 2>/dev/null | grep -q ."; then seen=1; break; fi
  sleep 2
done
q="$(gridware "squeue -h -j '$id' 2>/dev/null")"
if [ -n "$seen" ]; then pass "squeue lists job $id"; else fail "squeue never listed job $id"; fi
# squeue -h columns end with a mapped SLURM state (R/PD/CG...); just require a row.
assert_contains "$q" "$id" "squeue row carries the job id"

gridware "scancel '$id' >/dev/null 2>&1"
gone=""
for _ in $(seq 1 20); do
  gridware "squeue -h -j '$id' 2>/dev/null | grep -q ." || { gone=1; break; }
  sleep 2
done
if [ -n "$gone" ]; then pass "scancel removed the job from the queue"; else fail "job still present after scancel"; fi
gridware "qdel '$id' >/dev/null 2>&1 || true"
finish
