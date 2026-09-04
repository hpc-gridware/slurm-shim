#!/usr/bin/env bash
# Check: a granted RSMAP maps to SLURM_JOB_GPUS and per-rank CUDA_VISIBLE_DEVICES.
# Uses the fake RSMAP complex so no real GPU is needed.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "60_gpu: RSMAP grant -> SLURM_JOB_GPUS + per-rank CUDA_VISIBLE_DEVICES"

ensure_gpu_complex

job="$(mktemp)"
trap 'rm -f "$job"' EXIT
cat >"$job" <<'EOF'
#!/bin/bash
echo "GPUSONNODE=$SLURM_GPUS_ON_NODE"
echo "JOBGPUS=$SLURM_JOB_GPUS"
# Default binding: SLURM leaves the node's whole grant visible to every task.
srun -n 2 bash -c 'echo "default localid=$SLURM_LOCALID cuda=[$CUDA_VISIBLE_DEVICES]"'
# Explicit per-task binding still gives each rank its own device.
srun -n 2 --gpus-per-task=1 bash -c 'echo "pertask localid=$SLURM_LOCALID cuda=[$CUDA_VISIBLE_DEVICES]"'
EOF

remote=/home/gridware/e2e-60-gpu.sh
out=/home/gridware/e2e-60-gpu.out
put_job "$job" "$remote"
gridware "rm -f '$out'"
# gpu is a per-slot consumable: -l gpu=1 x 2 slots = both of the worker's devices.
id="$(gridware "qsub -terse -pe make 2 -l ${GPU_COMPLEX}=1 -q all.q@ocs-worker1 -o '$out' -j y '$remote'")"
id="${id%%.*}"
if [ -n "$id" ]; then pass "gpu job submitted (id $id)"; else fail "qsub returned no id"; fi

res="$(jobout "$id" "$out")"
assert_contains "$res" "GPUSONNODE=2" "SLURM_GPUS_ON_NODE == 2"
assert_contains "$res" "JOBGPUS=0,1" "SLURM_JOB_GPUS lists both devices"

# Delimited exact matches: a substring test cannot tell "0" from "0,1", which is
# precisely the distinction between the two binding models.
assert_contains "$res" "default localid=0 cuda=[0,1]" "unbound rank 0 sees the whole grant"
assert_contains "$res" "default localid=1 cuda=[0,1]" "unbound rank 1 sees the whole grant"
assert_contains "$res" "pertask localid=0 cuda=[0]" "--gpus-per-task binds rank 0 to one device"
assert_contains "$res" "pertask localid=1 cuda=[1]" "--gpus-per-task binds rank 1 to one device"

# The invariant JAX (local_device_ids=[SLURM_LOCALID]) and torch (LOCAL_RANK) need:
# every local rank must be a valid index into its own visible device list.
if printf '%s\n' "$res" | grep -q 'default localid=1 cuda=\[0,1\]'; then
  pass "SLURM_LOCALID indexes within each rank's visible devices"
else
  fail "a rank cannot index its own devices by SLURM_LOCALID"
fi
finish
