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
# Device ids are NOT asserted literally. On the container harness the RSMAP holds
# the ordinals 0 and 1; on a cluster with real devices it holds UUIDs, because
# ensure_gpu_complex refuses to overwrite a real RSMAP with invented ids. Both are
# correct, so the assertions are about the SHAPE of the grant -- how many devices
# a rank sees, and whether two ranks see the same ones -- which is what the two
# binding models actually differ in.
#
# This stays stricter than a substring test, not weaker: the bracket contents are
# parsed and counted, so "0" can never satisfy an expectation of "0,1".
cuda_of() { # cuda_of <prefix> <localid> -> comma-separated device list
  printf '%s\n' "$res" | sed -n "s/^$1 localid=$2 cuda=\\[\\([^]]*\\)\\].*/\\1/p" | head -1
}
count_of() { printf '%s' "$1" | tr ',' '\n' | sed '/^$/d' | grep -c .; }

u0="$(cuda_of default 0)"; u1="$(cuda_of default 1)"
p0="$(cuda_of pertask 0)"; p1="$(cuda_of pertask 1)"

assert_eq "$(count_of "$u0")" "2" "unbound rank 0 sees the whole grant (2 devices)"
assert_eq "$(count_of "$u1")" "2" "unbound rank 1 sees the whole grant (2 devices)"
assert_eq "$u0" "$u1" "both unbound ranks see the SAME two devices"

assert_eq "$(count_of "$p0")" "1" "--gpus-per-task binds rank 0 to exactly one device"
assert_eq "$(count_of "$p1")" "1" "--gpus-per-task binds rank 1 to exactly one device"
if [ -n "$p0" ] && [ "$p0" != "$p1" ]; then
  pass "the two bound ranks got DIFFERENT devices"
else
  fail "both bound ranks got the same device ($p0) -- per-task binding did not split the grant"
fi

# Exactly one device variable reaches a rank: on ROCm a second mask holding the
# same absolute ids indexes into the already-filtered list and selects the wrong
# devices, so a leaked ROCR_VISIBLE_DEVICES is not merely redundant.
ranks_total="$(printf '%s\n' "$res" | grep -c 'localid=' || true)"
ranks_unset="$(printf '%s\n' "$res" | grep -c 'rocr=\[UNSET\]' || true)"
# Guard the zero case FIRST. If the job produced no rank lines at all, both
# counts are 0 and the equality below passes while proving nothing -- the same
# vacuous-pass shape that already slipped through this suite once.
if [ "$ranks_total" -eq 0 ]; then
  fail "no rank lines in the job output -- nothing was verified about device variables"
else
  assert_eq "$ranks_unset" "$ranks_total" "all $ranks_total ranks have ROCR_VISIBLE_DEVICES unset (one vendor variable only)"
fi

# The invariant JAX (local_device_ids=[SLURM_LOCALID]) and torch (LOCAL_RANK) need:
# every local rank must be a valid index into its own visible device list.
if [ "$(count_of "$u1")" -gt 1 ]; then
  pass "SLURM_LOCALID indexes within each rank's visible devices"
else
  fail "a rank cannot index its own devices by SLURM_LOCALID"
fi
finish
