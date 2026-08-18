#!/usr/bin/env bash
# Check: the PE hook fabricates the allocation-level SLURM_* contract.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "10_env: SLURM_* fabrication across the allocation"

job="$(mktemp)"
trap 'rm -f "$job"' EXIT
cat >"$job" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=2
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh
echo "NNODES=$SLURM_NNODES"
echo "NTASKS=$SLURM_NTASKS"
echo "NODELIST=$SLURM_JOB_NODELIST"
echo "JOBID=$SLURM_JOB_ID"
echo "MASTER=$MASTER_ADDR:$MASTER_PORT"
EOF

remote=/home/gridware/e2e-10-env.sh
out=/home/gridware/e2e-10-env.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out")"
if [ -n "$id" ]; then pass "sbatch accepted the job (id $id)"; else fail "sbatch returned no job id"; fi

res="$(jobout "$id" "$out")"
assert_contains "$res" "NNODES=3" "SLURM_NNODES == 3"
assert_contains "$res" "NTASKS=6" "SLURM_NTASKS == 6"
assert_contains "$res" "ocs-master" "nodelist includes the master"
assert_contains "$res" "ocs-worker" "nodelist includes workers"
case "$res" in
  *JOBID=[0-9]*) pass "SLURM_JOB_ID is numeric" ;;
  *) fail "SLURM_JOB_ID missing/non-numeric" ;;
esac
finish
