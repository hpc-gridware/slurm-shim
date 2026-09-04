#!/usr/bin/env bash
# Check: --array end to end. SLURM array indices are 0-based and GE task ids are
# 1-based, so this pins the mapping against a REAL qsub rather than a fake: the
# job env, the srun-side %a expansion, sacct's ids, and the batch-level -o path.
# The %a-in-a-directory shape is the one Hydra's submitit launcher emits, which
# GE cannot express and used to fail every task with Eqw.
# Costs nothing to run: pure shell, no downloads.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "90_array: 0-based SLURM indices over 1-based GE tasks (env, srun, sacct, -o)"

work=/home/gridware/e2e-90-array
gridware "rm -rf '$work'; mkdir -p '$work/logs'"

# waitgone <jobid> blocks (bounded) until the array leaves the queue.
waitgone() {
  for _ in $(seq 1 90); do
    gridware "qstat 2>/dev/null | awk '{print \$1}' | grep -qx '$1'" || return 0
    sleep 2
  done
  return 1
}

# ---------------------------------------------------------------- 0-based array
# %a sits in a DIRECTORY component, so GE cannot be handed the path: the shim
# must substitute one rooted at the literal prefix (logs/) instead of failing.
job="$(mktemp)"; trap 'rm -f "$job"' EXIT
cat >"$job" <<EOF
#!/bin/bash
#SBATCH --partition=smp
#SBATCH --array=0-2
#SBATCH --output=$work/logs/%A_%a/task.out
echo "TASK slurm=\$SLURM_ARRAY_TASK_ID ge=\$SGE_TASK_ID count=\$SLURM_ARRAY_TASK_COUNT"
srun bash -c 'echo "RANK slurm=\$SLURM_ARRAY_TASK_ID procid=\$SLURM_PROCID"'
EOF
put_job "$job" "$work/array0.sh"

id="$(gridware "cd '$work' && sbatch array0.sh" | awk '/Submitted batch job/{print $NF}')"
if [ -n "$id" ]; then pass "0-based array submitted (id $id)"; else fail "sbatch returned no job id"; fi
waitgone "$id" || fail "array $id never left the queue"

# Nothing may sit in Eqw: that is the Hydra failure this check exists for.
if gridware "qstat -j '$id' 2>/dev/null | grep -qi 'error reason'"; then
  fail "array $id has an error reason (Eqw) -- the batch -o path was not expressible"
else
  pass "no task went to Eqw"
fi

res="$(gridware "cat '$work'/logs/* 2>/dev/null")"

# The job env must carry the SLURM index, not GE's. GE numbers 1..3.
assert_contains "$res" "TASK slurm=0 ge=1" "task 0 sees SLURM index 0 over GE task 1"
assert_contains "$res" "TASK slurm=1 ge=2" "task 1 sees SLURM index 1 over GE task 2"
assert_contains "$res" "TASK slurm=2 ge=3" "task 2 sees SLURM index 2 over GE task 3"
assert_contains "$res" "count=3" "SLURM_ARRAY_TASK_COUNT is the array size"

# srun inherits the same 0-based index inside the step.
assert_contains "$res" "RANK slurm=0" "srun step sees the 0-based index"

# The substituted batch path lands under the literal directory the user asked
# for -- never relocated to the submit dir.
if gridware "ls '$work'/logs/slurm-$id.1.out >/dev/null 2>&1"; then
  pass "batch-level output substituted into the requested directory"
else
  fail "expected $work/logs/slurm-$id.1.out (batch output was relocated or lost)"
fi

# sacct reports the SLURM 0-based element ids, which is what submitit/Hydra poll.
# The elements finish within milliseconds of each other and qacct lands their
# records a beat later than squeue drops them, so poll until all three rows are
# there rather than asserting on the first read (see docs/solutions/ on qacct
# briefly returning a partial set).
acct=""
for _ in $(seq 1 15); do
  acct="$(gridware "sacct -o JobID,State,NodeList --parsable2 -j '$id' 2>/dev/null")"
  [ "$(printf '%s\n' "$acct" | grep -c "^${id}_")" -ge 3 ] && break
  sleep 2
done
assert_contains "$acct" "${id}_0|" "sacct reports element 0 (0-based)"
assert_contains "$acct" "${id}_2|" "sacct reports element 2 (0-based)"

# ---------------------------------------------------------------- 1-based array
# Here GE's $TASK_ID lines up with %a, so the path passes through untouched and
# the files carry the user's own indices.
job2="$(mktemp)"; trap 'rm -f "$job" "$job2"' EXIT
cat >"$job2" <<EOF
#!/bin/bash
#SBATCH --partition=smp
#SBATCH --array=1-3
#SBATCH --output=$work/logs/aligned_%a.out
echo "ALIGNED slurm=\$SLURM_ARRAY_TASK_ID"
EOF
put_job "$job2" "$work/array1.sh"

id2="$(gridware "cd '$work' && sbatch array1.sh" | awk '/Submitted batch job/{print $NF}')"
if [ -n "$id2" ]; then pass "1-based array submitted (id $id2)"; else fail "sbatch returned no job id"; fi
waitgone "$id2" || fail "array $id2 never left the queue"

if gridware "ls '$work'/logs/aligned_1.out '$work'/logs/aligned_3.out >/dev/null 2>&1"; then
  pass "1-based %a passes through to GE \$TASK_ID (aligned_1..3 written)"
else
  fail "expected aligned_1.out..aligned_3.out from the pass-through path"
fi
assert_contains "$(gridware "cat '$work'/logs/aligned_2.out 2>/dev/null")" "ALIGNED slurm=2" \
  "1-based element 2 keeps its own index"

# %A_%a renders as $JOB_ID_$TASK_ID, i.e. a pseudo-variable immediately followed
# by an underscore. GE prefix-matches its pseudo-vars rather than reading a shell
# identifier, so this expands -- pin it, because a greedy parse would silently
# produce one literal filename for the whole array.
id4="$(gridware "cd '$work' && sbatch --array=1-2 --output='$work/logs/j_%A_%a.out' --wrap='echo adjacent'" \
  | awk '/Submitted batch job/{print $NF}')"
waitgone "$id4" || fail "array $id4 never left the queue"
if gridware "ls '$work'/logs/j_${id4}_1.out '$work'/logs/j_${id4}_2.out >/dev/null 2>&1"; then
  pass "an underscore-adjacent \$JOB_ID_\$TASK_ID expands per task"
else
  fail "expected j_${id4}_1.out and j_${id4}_2.out (pseudo-variable did not expand)"
fi

# scancel addresses an element by its 0-based SLURM index (submitit's form).
id3="$(gridware "cd '$work' && sbatch --array=0-1 --wrap='sleep 60'" | awk '/Submitted batch job/{print $NF}')"
sleep 4
gridware "scancel ${id3}_0" >/dev/null 2>&1
sleep 4
if gridware "qstat -t 2>/dev/null | grep -q '$id3'"; then
  pass "scancel of element 0 left the rest of the array running"
else
  skip "array $id3 already drained; element-cancel not observed"
fi
gridware "qdel '$id3'" >/dev/null 2>&1
gridware "rm -rf '$work'"
finish
