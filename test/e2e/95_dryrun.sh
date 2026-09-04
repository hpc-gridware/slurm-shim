#!/usr/bin/env bash
# Check: SLURM_SHIM_DRY_RUN / --test-only report without changing cluster state,
# and the environment they predict is the one the job actually gets.
#
# The local suite proves the prediction matches the fabricator. Only a live
# cluster can prove it matches REALITY -- the grant, the PE's allocation rule and
# the hook's exports are what the prediction is claiming to know. That parity
# assertion is the reason this check exists.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "95_dryrun: reports without mutating, and predicts what the job really gets"

job="$(mktemp)"
trap 'rm -f "$job"' EXIT
cat >"$job" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=2
#SBATCH --job-name=e2e-dryrun
env | grep '^SLURM_' | sort
EOF

remote=/home/gridware/e2e-95-dryrun.sh
out=/home/gridware/e2e-95-dryrun.out
put_job "$job" "$remote"

# ---------------------------------------------------------------- no mutation

before="$(gridware "qstat -u '*' 2>/dev/null | wc -l")"
dry_err="$(gridware "SLURM_SHIM_DRY_RUN=1 sbatch --output='$out' '$remote' 2>&1 >/dev/null")"
dry_out="$(gridware "SLURM_SHIM_DRY_RUN=1 sbatch --output='$out' '$remote' 2>/dev/null")"
after="$(gridware "qstat -u '*' 2>/dev/null | wc -l")"

assert_eq "$after" "$before" "a dry run queues no job [REQ-DRY-002]"
assert_contains "$dry_err" "dry run" "the banner names the mode"
assert_contains "$dry_err" "would submit" "the report shows the qsub line"
assert_contains "$dry_err" "qsub -terse -q all.q -pe make" "the qsub line names the mapped queue and PE"

case "$dry_out" in
  *"Submitted batch job"*) fail "stdout must not carry a submit line under a dry run" ;;
  *) pass "stdout carries no 'Submitted batch job' line [REQ-DRY-003]" ;;
esac
case "$dry_out" in
  *"dry run"*|*"would submit"*) fail "report prose leaked onto stdout" ;;
  *) pass "report prose stays on stderr [REQ-DRY-003]" ;;
esac

# --test-only must reach the same mode without an environment variable, since a
# caller that can only inject argv or #SBATCH lines has no other route.
t_before="$(gridware "qstat -u '*' 2>/dev/null | wc -l")"
t_err="$(gridware "sbatch --test-only --output='$out' '$remote' 2>&1 >/dev/null")"
t_after="$(gridware "qstat -u '*' 2>/dev/null | wc -l")"
assert_contains "$t_err" "dry run" "--test-only enters the mode with no env var"
assert_eq "$t_after" "$t_before" "--test-only queues no job"

# ------------------------------------------------------- the parity assertion

# Submit the same script for real and compare the environment it exports with the
# one the dry run predicted. Only keys the prediction claims to know are compared:
# <angle bracket> values are grant-derived by definition.
id="$(sbatch_submit "$remote" "$out")"
if [ -z "$id" ]; then
  fail "could not submit the reference job; skipping the parity assertion"
  finish
fi
pass "reference job submitted ($id)"
real="$(jobout "$id" "$out")"

if [ -z "$real" ]; then
  fail "reference job produced no output; skipping the parity assertion"
  finish
fi

mismatch=""
checked=0
while IFS='=' read -r key val; do
  case "$key" in
    ""|*" "*) continue ;;
  esac
  case "$val" in
    "<"*) continue ;;                 # resolved from the grant, not predictable
  esac
  # Compare only keys the real job actually exported.
  got="$(printf '%s\n' "$real" | awk -F= -v k="$key" '$1==k {sub(/^[^=]*=/, ""); print; exit}')"
  [ -z "$got" ] && continue
  checked=$((checked + 1))
  if [ "$got" != "$val" ]; then
    mismatch="$mismatch $key(pred=$val real=$got)"
  fi
done <<EOF
$dry_out
EOF

if [ "$checked" -lt 5 ]; then
  fail "parity compared only $checked keys -- the prediction looks empty"
elif [ -n "$mismatch" ]; then
  fail "predicted environment differs from the job's:$mismatch"
else
  pass "predicted environment matches the real job on all $checked comparable keys [REQ-DRY-005]"
fi

# The headline numbers specifically, so a failure names them rather than hiding
# in the loop above.
for k in SLURM_NTASKS SLURM_JOB_NUM_NODES SLURM_TASKS_PER_NODE; do
  p="$(printf '%s\n' "$dry_out" | awk -F= -v k="$k" '$1==k {sub(/^[^=]*=/, ""); print; exit}')"
  r="$(printf '%s\n' "$real" | awk -F= -v k="$k" '$1==k {sub(/^[^=]*=/, ""); print; exit}')"
  if [ -n "$p" ] && [ -n "$r" ]; then assert_eq "$p" "$r" "$k predicted correctly"; fi
done

# --------------------------------------------------- the switch fails open

# An unrecognized value must leave the real behavior in place: the mode's on-state
# suppresses work, so 'n' meaning "yes, dry run" would turn a job into a no-op.
n_id="$(gridware "SLURM_SHIM_DRY_RUN=n sbatch --output=/dev/null '$remote'" | awk '/Submitted batch job/{print $NF}')"
if [ -n "$n_id" ]; then
  pass "SLURM_SHIM_DRY_RUN=n submits for real [REQ-DRY-001]"
  gridware "scancel '$n_id' >/dev/null 2>&1"
else
  fail "SLURM_SHIM_DRY_RUN=n did not submit -- the switch failed closed"
fi

# ------------------------------------------- the variable cannot reach a job

# qsub -V forwards the submit environment, and the fabricator's unset preamble is
# what stops an inherited dry-run flag turning every srun in the job into a no-op
# that still exits 0. Plant it explicitly and confirm the job does real work.
leak_out=/home/gridware/e2e-95-leak.out
leak="$(mktemp)"
cat >"$leak" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=1
#SBATCH --ntasks-per-node=1
srun echo REAL-WORK-RAN
EOF
leak_remote=/home/gridware/e2e-95-leak.sh
put_job "$leak" "$leak_remote"
rm -f "$leak"

leak_id="$(gridware "rm -f '$leak_out'; sbatch --export=ALL,SLURM_SHIM_DRY_RUN=1 --output='$leak_out' '$leak_remote'" \
  | awk '/Submitted batch job/{print $NF}')"
if [ -z "$leak_id" ]; then
  fail "could not submit the leak-check job"
else
  leak_res="$(jobout "$leak_id" "$leak_out")"
  assert_contains "$leak_res" "REAL-WORK-RAN" \
    "an inherited SLURM_SHIM_DRY_RUN does not silence srun inside the job [REQ-DRY-005]"
fi

# No-starter arm: a job that sources the hook ITSELF (the documented fallback for
# sites without a queue starter_method) must scrub the inherited DRY_RUN flag too
# -- the scrub lives in the fabricated environment file, which the hook sources
# whichever path reaches it (todos/037).
leak2_out=/home/gridware/e2e-95-leak2.out
leak2="$(mktemp)"
cat >"$leak2" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=1
#SBATCH --ntasks-per-node=1
. /opt/slurm-shim/etc/slurm-shim-source-hook.sh
srun echo REAL-WORK-RAN-NOSTARTER
EOF
leak2_remote=/home/gridware/e2e-95-leak2.sh
put_job "$leak2" "$leak2_remote"
rm -f "$leak2"
leak2_id="$(gridware "rm -f '$leak2_out'; sbatch --export=ALL,SLURM_SHIM_DRY_RUN=1 --output='$leak2_out' '$leak2_remote'" \
  | awk '/Submitted batch job/{print $NF}')"
if [ -z "$leak2_id" ]; then
  fail "could not submit the no-starter leak-check job"
else
  leak2_res="$(jobout "$leak2_id" "$leak2_out")"
  assert_contains "$leak2_res" "REAL-WORK-RAN-NOSTARTER" \
    "the scrub reaches a job that sources the hook itself (no-starter fallback) [REQ-DRY-005]"
fi

# ------------------------------------------------- scancel changes nothing

# A dry-run scancel that reported success while leaving the job running is how an
# orchestrator ends up resubmitting work that is still holding its slots.
keep="$(mktemp)"
cat >"$keep" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=1
#SBATCH --ntasks-per-node=1
sleep 120
EOF
keep_remote=/home/gridware/e2e-95-keep.sh
put_job "$keep" "$keep_remote"
rm -f "$keep"

keep_id="$(gridware "sbatch --output=/dev/null '$keep_remote'" | awk '/Submitted batch job/{print $NF}')"
if [ -z "$keep_id" ]; then
  fail "could not submit the scancel-check job"
else
  c_err="$(gridware "SLURM_SHIM_DRY_RUN=1 scancel '$keep_id' 2>&1 >/dev/null")"
  assert_contains "$c_err" "would run: qdel $keep_id" "scancel reports the qdel it would run"
  sleep 2
  if gridware "qstat -u '*' 2>/dev/null | grep -q '^ *$keep_id '"; then
    pass "the job is still there -- a dry-run scancel cancelled nothing [REQ-DRY-002]"
  else
    fail "the job is gone -- a dry-run scancel actually cancelled it"
  fi
  gridware "qdel '$keep_id' >/dev/null 2>&1"
fi

finish
