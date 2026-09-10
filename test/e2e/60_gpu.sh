#!/usr/bin/env bash
# Check: a granted RSMAP maps to SLURM_JOB_GPUS and per-rank CUDA_VISIBLE_DEVICES.
# Uses the fake RSMAP complex so no real GPU is needed.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
# Target a host that actually declares devices instead of naming a container,
# so this runs against a real cluster too.
GPU_HOST="$(gpu_host)"
[ -n "$GPU_HOST" ] || { fail "no exec host declares the $GPU_COMPLEX complex"; finish; }
log "60_gpu: RSMAP grant -> SLURM_JOB_GPUS + per-rank CUDA_VISIBLE_DEVICES"

ensure_gpu_complex

job="$(mktemp)"
trap 'rm -f "$job"' EXIT
cat >"$job" <<'EOF'
#!/bin/bash
echo "GPUSONNODE=$SLURM_GPUS_ON_NODE"
echo "JOBGPUS=$SLURM_JOB_GPUS"
# Default binding: SLURM leaves the node's whole grant visible to every task.
srun -n 2 bash -c 'echo "default localid=$SLURM_LOCALID cuda=[$CUDA_VISIBLE_DEVICES] rocr=[${ROCR_VISIBLE_DEVICES-UNSET}]"'
# Explicit per-task binding still gives each rank its own device.
srun -n 2 --gpus-per-task=1 bash -c 'echo "pertask localid=$SLURM_LOCALID cuda=[$CUDA_VISIBLE_DEVICES] rocr=[${ROCR_VISIBLE_DEVICES-UNSET}]"'
EOF

remote=$JOB_HOME/e2e-60-gpu.sh
out=$JOB_HOME/e2e-60-gpu.out
put_job "$job" "$remote"
gridware "rm -f '$out'"
# gpu is a per-slot consumable: -l gpu=1 x 2 slots = both of the worker's devices.
id="$(gridware "qsub -terse -pe make 2 -l ${GPU_COMPLEX}=1 -q all.q@$GPU_HOST -o '$out' -j y '$remote'")"
id="${id%%.*}"
if [ -n "$id" ]; then pass "gpu job submitted (id $id)"; else fail "qsub returned no id"; fi

res="$(jobout "$id" "$out")"
assert_contains "$res" "GPUSONNODE=2" "SLURM_GPUS_ON_NODE == 2"
assert_contains "$res" "JOBGPUS=0,1" "SLURM_JOB_GPUS lists both devices"

# Delimited exact matches: a substring test cannot tell "0" from "0,1", which is
# precisely the distinction between the two binding models.
# Whole lines, including rocr: a bare "rocr=[UNSET]" would pass while one of the
# two ranks leaked, and a bare "cuda=[0]" prefix-matches "cuda=[0,1]". Exactly one
# device variable reaches a rank -- on ROCm a second mask holding the same
# absolute ids indexes into the already-filtered list and selects wrong devices.
assert_contains "$res" "default localid=0 cuda=[0,1] rocr=[UNSET]" "unbound rank 0 sees the whole grant, and only that variable"
assert_contains "$res" "default localid=1 cuda=[0,1] rocr=[UNSET]" "unbound rank 1 sees the whole grant, and only that variable"
assert_contains "$res" "pertask localid=0 cuda=[0] rocr=[UNSET]" "--gpus-per-task binds rank 0 to one device, and only that variable"
assert_contains "$res" "pertask localid=1 cuda=[1] rocr=[UNSET]" "--gpus-per-task binds rank 1 to one device, and only that variable"

# The invariant JAX (local_device_ids=[SLURM_LOCALID]) and torch (LOCAL_RANK) need:
# every local rank must be a valid index into its own visible device list.
if printf '%s\n' "$res" | grep -q 'default localid=1 cuda=\[0,1\]'; then
  pass "SLURM_LOCALID indexes within each rank's visible devices"
else
  fail "a rank cannot index its own devices by SLURM_LOCALID"
fi
finish
