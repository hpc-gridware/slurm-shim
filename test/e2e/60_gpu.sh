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
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh
echo "GPUSONNODE=$SLURM_GPUS_ON_NODE"
echo "JOBGPUS=$SLURM_JOB_GPUS"
srun -n 2 bash -c 'echo "cuda=$CUDA_VISIBLE_DEVICES rank=$SLURM_PROCID"'
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
assert_contains "$res" "cuda=0" "a rank sees CUDA device 0"
assert_contains "$res" "cuda=1" "a rank sees CUDA device 1"
finish
