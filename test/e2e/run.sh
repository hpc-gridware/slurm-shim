#!/usr/bin/env bash
# Run the e2e suite against a running slurm-shim cluster (make cluster-up first).
# Each NN_*.sh check is a self-contained process that exits non-zero on failure;
# this orchestrator runs them in order and fails if any did.
set -uo pipefail
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$E2E_DIR/e2e-lib.sh"

require_cluster

checks=(
  05_hook
  10_env
  20_srun
  30_sbatch
  31_sbatch_resources
  32_par_allocation
  40_squeue_scancel
  50_scontrol
  60_gpu
  70_reject
  80_sinfo
  90_array
  91_sacct
  95_dryrun
)

fails=0
for c in "${checks[@]}"; do
  printf '\n\033[1;36m== %s ==\033[0m\n' "$c"
  bash "$E2E_DIR/$c.sh" || fails=$((fails + 1))
done

printf '\n'
if [ "$fails" -gt 0 ]; then
  die "$fails e2e check(s) failed"
fi
log "all e2e checks passed"
