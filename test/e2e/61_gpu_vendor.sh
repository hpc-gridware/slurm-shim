#!/usr/bin/env bash
# Check: gpu.vendor selects which device variable a rank receives, the other
# vendor's variables are removed rather than left to layer on top, an RSMAP UUID
# survives to the rank verbatim, and an unknown vendor refuses only GPU steps.
#
# No AMD hardware is needed: this exercises the environment contract, which is
# where the failure lives. On ROCm the HIP-level masks index into the list
# ROCR_VISIBLE_DEVICES already filtered, so an inherited CUDA_VISIBLE_DEVICES
# left beside the ROCr mask selects the wrong devices, or none, with no error.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
# Target a host that actually declares devices instead of naming a container,
# so this runs against a real cluster too.
GPU_HOST="$(gpu_host)"
[ -n "$GPU_HOST" ] || { fail "no exec host declares the $GPU_COMPLEX complex"; finish; }
log "61_gpu_vendor: gpu.vendor selects and isolates the device variable"

ensure_gpu_complex

SHIM_CFG="$(manager 'echo $SGE_ROOT/$SGE_CELL/common/slurm-shim/config.yaml')"
SHIM_CFG="$(echo "$SHIM_CFG" | tr -d '\r')"
if [ -z "$SHIM_CFG" ] || ! manager "test -f '$SHIM_CFG'"; then
  skip "shim config not found on the master; run 'make cluster-install' first"
  finish
fi

# Always put the cluster back, even on an early failure: later checks read this
# config, and a stray vendor would fail them for the wrong reason.
restore() {
  manager "test -f '$SHIM_CFG.e2ebak' && mv '$SHIM_CFG.e2ebak' '$SHIM_CFG'" >/dev/null 2>&1
  ensure_gpu_complex >/dev/null 2>&1
}
trap restore EXIT
manager "cp '$SHIM_CFG' '$SHIM_CFG.e2ebak'"

# set_vendor verifies itself: sed exits 0 when nothing matches, and a silent
# no-op would leave the nvidia phase passing for the wrong reason, since nvidia is
# also the default.
set_vendor() {
  manager "sed -i 's/^\( *\)vendor: .*/\1vendor: $1/' '$SHIM_CFG'"
  if ! manager "grep -qE '^[[:space:]]*vendor:[[:space:]]*$1\$' '$SHIM_CFG'"; then
    fail "could not set gpu.vendor to $1 in $SHIM_CFG"
    finish
  fi
}

job="$(mktemp)"
trap 'rm -f "$job"' RETURN 2>/dev/null || true
cat >"$job" <<'EOF'
#!/bin/bash
echo "ALLOC JOBGPUS=[$SLURM_JOB_GPUS]"
srun -n 2 --gpus-per-task=1 bash -c 'echo "RANK $SLURM_LOCALID cuda=[${CUDA_VISIBLE_DEVICES-UNSET}] rocr=[${ROCR_VISIBLE_DEVICES-UNSET}] hip=[${HIP_VISIBLE_DEVICES-UNSET}] ord=[${GPU_DEVICE_ORDINAL-UNSET}]"'
EOF
remote=$JOB_HOME/e2e-61-vendor.sh
put_job "$job" "$remote"

cat >"$job" <<'EOF'
#!/bin/bash
echo "ALLOC JOBGPUS=[$SLURM_JOB_GPUS]"
srun -n 2 bash -c 'echo "RANK $SLURM_LOCALID cuda=[${CUDA_VISIBLE_DEVICES-UNSET}] rocr=[${ROCR_VISIBLE_DEVICES-UNSET}]"'
EOF
remote_unbound=$JOB_HOME/e2e-61-vendor-unbound.sh
put_job "$job" "$remote_unbound"
rm -f "$job"

# run_gpu_job <outfile-tag> -> prints the job output. The submit environment
# carries foreign device masks, as a site prolog or container image would leave
# them; ids differ from the grant so a leak is visible in the output.
run_gpu_job() {
  local out="$JOB_HOME/e2e-61-$1.out" script="$remote" id
  [ "${2-}" = unbound ] && script="$remote_unbound"
  gridware "rm -f '$out'"
  id="$(gridware "CUDA_VISIBLE_DEVICES=6,7 HIP_VISIBLE_DEVICES=6,7 GPU_DEVICE_ORDINAL=6,7 qsub -terse -V -pe make 2 -l ${GPU_COMPLEX}=1 -q all.q@$GPU_HOST -o '$out' -j y '$script'")"
  id="${id%%.*}"
  jobout "$id" "$out"
}

# Device ids are asserted by SHAPE, not literally. The container harness
# publishes ordinals (0 1); a cluster with real devices publishes UUIDs, and a
# literal "cuda=[0]" failed there while every rank had the right device. What
# the vendor switch changes is WHICH variable holds the ids and how many each
# rank gets, so that is what is checked.
#
# field_of <rank> <var>: the bracket contents of var on that rank's line of $res.
field_of() {
  printf '%s\n' "$res" | sed -n "s/^RANK $1 .*$2=\[\([^]]*\)\].*/\1/p" | head -1
}
# count_ids <list>: how many ids a comma list holds; UNSET and empty hold none.
count_ids() {
  case "$1" in ''|UNSET) echo 0; return ;; esac
  printf '%s' "$1" | tr ',' '\n' | sed '/^$/d' | grep -c . || true
}
# assert_one_each_distinct <var> <label>: each rank holds exactly one id in var,
# and the two ranks hold different ones.
assert_one_each_distinct() {
  local r0 r1
  r0="$(field_of 0 "$1")"; r1="$(field_of 1 "$1")"
  assert_eq "$(count_ids "$r0")" "1" "$2: rank 0 gets exactly one device in $1"
  assert_eq "$(count_ids "$r1")" "1" "$2: rank 1 gets exactly one device in $1"
  if [ "$(count_ids "$r0")" = 1 ] && [ "$r0" != "$r1" ]; then
    pass "$2: the two ranks got different devices"
  else
    fail "$2: ranks got the same device, or none ($r0 / $r1)"
  fi
}

# --- nvidia (the default) ---------------------------------------------------
set_vendor nvidia
res="$(run_gpu_job nvidia)"
assert_one_each_distinct cuda "nvidia"
assert_eq "$(field_of 0 rocr)$(field_of 1 rocr)" "UNSETUNSET" "nvidia: no ROCR_VISIBLE_DEVICES is written"

# --- amd --------------------------------------------------------------------
set_vendor amd
res="$(run_gpu_job amd)"
assert_one_each_distinct rocr "amd"
assert_eq "$(field_of 0 cuda)$(field_of 1 cuda)" "UNSETUNSET" "amd: no CUDA_VISIBLE_DEVICES is written"
# The point of the feature: an inherited mask must be gone, not merely shadowed.
if printf '%s\n' "$res" | grep -q 'RANK .*hip=\[UNSET\] ord=\[UNSET\]'; then
  pass "amd: inherited HIP_VISIBLE_DEVICES and GPU_DEVICE_ORDINAL are removed"
else
  fail "amd: an inherited HIP-level mask survived into the rank"
fi
if printf '%s\n' "$res" | grep -q 'cuda=\[6,7\]'; then
  fail "amd: the inherited CUDA_VISIBLE_DEVICES reached the rank"
else
  pass "amd: the inherited CUDA_VISIBLE_DEVICES was removed"
fi

# The invariant frameworks depend on: every local rank must be a valid index
# into its own visible device list. JAX reads local_device_ids=[SLURM_LOCALID]
# and torch reads LOCAL_RANK, so an unbound rank must see the whole grant.
res="$(run_gpu_job amdunbound unbound)"
u0="$(field_of 0 rocr)"; u1="$(field_of 1 rocr)"
assert_eq "$(count_ids "$u0")" "2" "amd unbound: rank 0 sees the whole grant"
assert_eq "$(count_ids "$u1")" "2" "amd unbound: rank 1 sees the whole grant"
assert_eq "$u0" "$u1" "amd unbound: both ranks see the same two devices"
assert_eq "$(field_of 0 cuda)$(field_of 1 cuda)" "UNSETUNSET" "amd unbound: no CUDA_VISIBLE_DEVICES is written"

# --- amd with UUID RSMAP ids ------------------------------------------------
# A device UUID is the only identity stable across reboot, driver reload and an
# AMD compute-partition change. Device ids used to be coerced to int, so a UUID
# silently became its position in the grant and the job got the wrong devices.
manager "qconf -mattr exechost complex_values '${GPU_COMPLEX}=2(GPU-0123456789abcdef GPU-dead0000000000ff)' $GPU_HOST" >/dev/null
res="$(run_gpu_job uuid)"
assert_contains "$res" "rocr=[GPU-0123456789abcdef]" "uuid: the first UUID reaches its rank verbatim"
assert_contains "$res" "rocr=[GPU-dead0000000000ff]" "uuid: the second UUID reaches its rank verbatim"
# SLURM documents SLURM_JOB_GPUS as global ids; a UUID map has none, so the
# grant position is reported and the variable stays numeric.
assert_contains "$res" "ALLOC JOBGPUS=[0,1]" "uuid: SLURM_JOB_GPUS stays numeric"
ensure_gpu_complex >/dev/null

# --- an unknown vendor refuses GPU steps only -------------------------------
set_vendor intel
res="$(run_gpu_job intel)"
# The srun prefix specifically. The bare string also appears in the config
# warning, which the PE hook emits at allocation time and which says nothing
# about whether the step was refused.
assert_contains "$res" "srun: error: unknown gpu.vendor" "srun refuses the step, not merely warns"
# The allocation still ran AND produced a grant: config validation must never fail
# the PE hook, or a typo would take down every job on the host, GPU or not. A bare
# "JOBGPUS=" would also match an empty grant.
assert_contains "$res" "ALLOC JOBGPUS=[0,1]" "unknown vendor does not fail the allocation itself"
# Any device list in CUDA_VISIBLE_DEVICES counts, not just "[0]": a literal id
# can never match a UUID RSMAP, which made this pass on real hardware whatever
# the shim did.
if printf '%s\n' "$res" | grep -q '^RANK .*cuda=\[[^]U][^]]*\]'; then
  fail "unknown vendor silently fell back to CUDA_VISIBLE_DEVICES"
else
  pass "unknown vendor refuses the GPU step instead of guessing a variable"
fi

restore
trap - EXIT
finish
