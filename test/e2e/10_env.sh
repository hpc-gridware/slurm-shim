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
# Deliberately the ONE e2e job that still sources the hook itself. Every other
# check submits a pristine script and relies on the queue starter_method; this
# one keeps the explicit line so the no-starter fallback stays covered, and so
# sourcing twice (starter, then script) is shown to be harmless.
. @SHIM_PREFIX@/etc/slurm-shim-source-hook.sh
echo "NNODES=$SLURM_NNODES"
echo "NTASKS=$SLURM_NTASKS"
echo "NODELIST=$SLURM_JOB_NODELIST"
echo "JOBID=$SLURM_JOB_ID"
echo "MASTER=$MASTER_ADDR:$MASTER_PORT"
EOF
# The heredoc is quoted so every $SLURM_* reaches the job untouched; splice the
# site's install prefix in afterwards.
sed -i.bak "s|@SHIM_PREFIX@|$SHIM_PREFIX|" "$job" && rm -f "$job.bak"

remote=$JOB_HOME/e2e-10-env.sh
out=$JOB_HOME/e2e-10-env.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out")"
if [ -n "$id" ]; then pass "sbatch accepted the job (id $id)"; else fail "sbatch returned no job id"; fi

res="$(jobout "$id" "$out")"
assert_contains "$res" "NNODES=3" "SLURM_NNODES == 3"
assert_contains "$res" "NTASKS=6" "SLURM_NTASKS == 6"
# Every name in the nodelist must be a node of THIS cluster -- portable, and a
# stronger claim than matching a container-name prefix.
# SLURM_JOB_NODELIST is COMPRESSED ("host[2,1]"), so it must be expanded before
# the names can be checked -- splitting on commas would tear the bracket apart.
nl_raw="$(printf '%s\n' "$res" | sed -n 's/^NODELIST=//p')"
nl="$(gridware "scontrol show hostnames '$nl_raw'" | tr '\n' ' ')"
unknown=""
for h in $nl; do
  case " ${NODES[*]} " in *" $h "*) ;; *) unknown="$unknown $h" ;; esac
done
if [ -n "$nl" ] && [ -z "$unknown" ]; then
  pass "every host in the nodelist belongs to this cluster ($nl)"
else
  fail "nodelist '$nl_raw' expanded to '$nl'; unknown hosts:${unknown:-<empty>}"
fi
case "$res" in
  *JOBID=[0-9]*) pass "SLURM_JOB_ID is numeric" ;;
  *) fail "SLURM_JOB_ID missing/non-numeric" ;;
esac
finish
