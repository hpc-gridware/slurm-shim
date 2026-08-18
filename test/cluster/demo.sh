#!/usr/bin/env bash
# Submit a demo job to the running cluster and print its output.
#   demo.sh cpu   -> multi-node srun fan-out via the shim sbatch
#   demo.sh gpu   -> per-rank CUDA_VISIBLE_DEVICES from a fake RSMAP grant
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_cluster

mode="${1:-cpu}"
out="/home/gridware/shim-demo-$mode.out"
job="/home/gridware/shim-demo-$mode.sh"
docker cp "$CLUSTER_DIR/jobs/demo-$mode.sh" "$MASTER:$job"
docker exec "$MASTER" chown gridware:gridware "$job"
gridware "rm -f '$out'"

if [ "$mode" = gpu ]; then
  # gpu is a per-slot consumable (consumable=YES): -l gpu=1 books one device per
  # slot, so -pe make 2 lands 2 slots x 1 GPU = both of worker1's devices, one per
  # rank. Requesting gpu=2 here would demand 2 devices *per slot* and never place.
  log "submitting GPU demo: qsub -pe make 2 -l ${GPU_COMPLEX}=1 -q all.q@ocs-worker1"
  id="$(gridware "qsub -terse -pe make 2 -l ${GPU_COMPLEX}=1 -q all.q@ocs-worker1 -o '$out' -j y '$job'")"
else
  log "submitting CPU demo through the shim sbatch"
  id="$(gridware "sbatch --output='$out' '$job'" | awk '{print $NF}')"
fi
id="${id%%.*}"   # strip any array suffix
log "job $id submitted; waiting for it to finish"

for _ in $(seq 1 80); do
  gridware "squeue -h -j '$id' 2>/dev/null | grep -q ." || break
  sleep 3
done

echo
log "---- job $id output ----"
gridware "cat '$out' 2>/dev/null" || echo "(no output file; check: docker exec -u gridware $MASTER qstat -j $id)"
echo
