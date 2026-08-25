#!/usr/bin/env bash
# Submit a demo job to the running cluster and print its output.
#   demo.sh cpu   -> multi-node srun fan-out via the shim sbatch
#   demo.sh gpu   -> per-rank CUDA_VISIBLE_DEVICES from a fake RSMAP grant
#   demo.sh flax  -> multi-node data-parallel Flax training over one global mesh
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_cluster

mode="${1:-cpu}"
out="/home/gridware/shim-demo-$mode.out"
job="/home/gridware/shim-demo-$mode.sh"
docker cp "$CLUSTER_DIR/jobs/demo-$mode.sh" "$MASTER:$job"
docker exec "$MASTER" chown gridware:gridware "$job"
gridware "rm -f '$out'"

submit_env=""
if [ "$mode" = flax ]; then
  # The venv and the workload both live on the shared home, so every node runs the
  # same python and the same script (srun does not ship files). PYTHON_BIN rides
  # along in the submit environment (sbatch forwards it, --export=ALL) so an
  # overridden FLAX_VENV reaches the job instead of only the venv builder.
  ensure_flax_env
  docker cp "$REPO_ROOT/docs/recipes/flax/flax_dp_train.py" "$MASTER:/home/gridware/flax_dp_train.py"
  docker exec "$MASTER" chown gridware:gridware /home/gridware/flax_dp_train.py
  submit_env="PYTHON_BIN='$FLAX_VENV/bin/python' "
fi

if [ "$mode" = gpu ]; then
  # gpu is a per-slot consumable (consumable=YES): -l gpu=1 books one device per
  # slot, so -pe make 2 lands 2 slots x 1 GPU = both of worker1's devices, one per
  # rank. Requesting gpu=2 here would demand 2 devices *per slot* and never place.
  log "submitting GPU demo: qsub -pe make 2 -l ${GPU_COMPLEX}=1 -q all.q@ocs-worker1"
  id="$(gridware "qsub -terse -pe make 2 -l ${GPU_COMPLEX}=1 -q all.q@ocs-worker1 -o '$out' -j y '$job'")"
else
  log "submitting $mode demo through the shim sbatch"
  id="$(gridware "${submit_env}sbatch --output='$out' '$job'" | awk '{print $NF}')"
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
