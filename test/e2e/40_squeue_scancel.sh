#!/usr/bin/env bash
# Check: squeue lists a live job (GE state -> SLURM state mapping) and scancel
# removes it.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "40_squeue_scancel: squeue lists a job, scancel removes it"

job="$(mktemp)"
trap 'rm -f "$job"' EXIT
cat >"$job" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=1
#SBATCH --ntasks-per-node=1
sleep 120
EOF

remote=$JOB_HOME/e2e-40-sleep.sh
out=$JOB_HOME/e2e-40-sleep.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out")"

# Wait until squeue reports the job at all (queued or running).
seen=""
for _ in $(seq 1 20); do
  if gridware "squeue -h -j '$id' 2>/dev/null | grep -q ."; then seen=1; break; fi
  sleep 2
done
q="$(gridware "squeue -h -j '$id' 2>/dev/null")"
if [ -n "$seen" ]; then pass "squeue lists job $id"; else fail "squeue never listed job $id"; fi
# squeue -h columns end with a mapped SLURM state (R/PD/CG...); just require a row.
assert_contains "$q" "$id" "squeue row carries the job id"

# The node columns must describe the GRANTED allocation, not the request. Three
# of the eight default columns used to be placeholders: TIME was always 0:00,
# NODES always 1, and NODELIST showed only the master task's host, so a job
# spanning three nodes read as one.
#
# The oracle is `qhost -j`, deliberately NOT the view the shim reads. squeue
# sources its hosts from `qstat -xml -j`; an oracle taken from that same output
# would carry any distortion in the source identically on both sides of the
# assertion and cancel out. qhost is host-oriented: it prints each host as a
# section heading -- untruncated, since it is not a job column -- and lists the
# jobs under it.
multi="$(mktemp)"
cat >"$multi" <<'EOF'
#!/bin/bash
sleep 120
EOF
mremote=$JOB_HOME/e2e-40-multi.sh
put_job "$multi" "$mremote"
rm -f "$multi"
mid="$(gridware "qsub -terse -N e2e40multi -pe make 3 -q all.q '$mremote'")"
mid="${mid%%.*}"
for _ in $(seq 1 30); do
  gridware "qstat -g t -u '*' 2>/dev/null | grep -q '^ *$mid .*MASTER'" && break
  sleep 2
done

# Ground truth: the distinct hosts whose qhost section lists this job.
truth_hosts="$(gridware "qhost -j 2>/dev/null" | awk -v j="$mid" '
  /^[^ \t]/ { host = $1; next }
  $1 == j && !(host in seen) { seen[host]; print host }' | sort | tr '\n' ' ')"
truth_hosts="${truth_hosts% }"
truth_count="$(printf '%s' "$truth_hosts" | wc -w | tr -d ' ')"

got_nodes="$(gridware "squeue -h -j '$mid' -o '%D' 2>/dev/null" | tr -d ' ')"
got_list="$(gridware "squeue -h -j '$mid' -o '%N' 2>/dev/null" | tr -d ' ')"

if [ "$truth_count" -lt 2 ]; then
  # The scheduler packed the job onto one host, so every assertion below is
  # satisfied by the old placeholder too. Reporting PASS here would be a lie.
  skip "squeue node columns: scheduler packed job $mid onto $truth_count host(s), cannot distinguish the fix"
else
  assert_eq "$got_nodes" "$truth_count" "squeue NODES matches the granted host count"
  # Expanding the compressed list and comparing the SET is what separates the fix
  # from the bug: the old behaviour produced the master host alone, which is
  # non-empty and not "(None)" and so satisfied the previous assertion.
  got_hosts="$(gridware "scontrol show hostnames '$got_list' 2>/dev/null" | sort | tr '\n' ' ')"
  got_hosts="${got_hosts% }"
  assert_eq "$got_hosts" "$truth_hosts" "squeue NODELIST names exactly the granted hosts"
fi

# TIME must advance for a running job rather than sitting at the old 0:00.
#
# Poll, don't sample once. The wait above ends as soon as qstat lists the MASTER
# task, which it does while the job is still in state t, about a second before
# its recorded start time; TIME then correctly reads 0:00 for the first seconds.
# A single immediate sample failed every nightly run on fast CI runners while
# passing on a laptop. The placeholder never advances, so polling still catches it.
got_time=""
for _ in $(seq 1 15); do
  got_time="$(gridware "squeue -h -j '$mid' -o '%M' 2>/dev/null" | tr -d ' ')"
  [ -n "$got_time" ] && [ "$got_time" != "0:00" ] && break
  sleep 1
done
if [ -n "$got_time" ] && [ "$got_time" != "0:00" ]; then
  pass "squeue TIME reports elapsed run time ($got_time)"
else
  fail "squeue TIME is still the 0:00 placeholder"
fi

# A job name containing a space is what forced the move off the `qstat -g t`
# text view: it shifted every column there and made squeue name a host the job
# was not on. sbatch --job-name forwards such a name verbatim, so assert the
# shim still reads the right hosts with one in the listing.
sid="$(gridware "qsub -terse -N 'my job' -q all.q -b y sleep 120")"
sid="${sid%%.*}"
for _ in $(seq 1 20); do
  gridware "qstat -u '*' 2>/dev/null | grep -q '^ *$sid .* r '" && break
  sleep 2
done
if [ "$truth_count" -ge 2 ]; then
  after="$(gridware "squeue -h -j '$mid' -o '%N' 2>/dev/null" | tr -d ' ')"
  assert_eq "$after" "$got_list" "a spaced job name does not corrupt another job's node list"
fi
gridware "qdel '$sid' >/dev/null 2>&1 || true"

gridware "qdel '$mid' >/dev/null 2>&1"

# Filters intersect with the ids, as in SLURM: another user's filter leaves the
# job alone (it used to qdel -u that user's whole job set instead).
gridware "scancel -u root '$id' >/dev/null 2>&1 || true"
if gridware "squeue -h -j '$id' 2>/dev/null | grep -q ."; then
  pass "scancel -u <other user> <id> leaves a job it does not own"
else
  fail "scancel -u <other user> <id> cancelled the job"
fi
# An untranslated option steps over its value: --qos normal does not become the
# script (it used to be submitted as one).
qid="$(gridware "cd '$JOB_HOME' && sbatch --parsable --qos normal '$remote' 2>/dev/null" | tr -d '\r' | tail -1)"
case "$qid" in
  ''|*[!0-9]*) fail "sbatch --qos normal <script> did not submit the script (got: $qid)" ;;
  *) pass "sbatch --qos normal <script> submits the script"; gridware "qdel '$qid' >/dev/null 2>&1 || true" ;;
esac

gridware "scancel '$id' >/dev/null 2>&1"
gone=""
for _ in $(seq 1 20); do
  gridware "squeue -h -j '$id' 2>/dev/null | grep -q ." || { gone=1; break; }
  sleep 2
done
if [ -n "$gone" ]; then pass "scancel removed the job from the queue"; else fail "job still present after scancel"; fi
gridware "qdel '$id' >/dev/null 2>&1 || true"

# A job that is already gone is not an error: SLURM's scancel exits 0 and says
# nothing for a finished job and for an unknown id alike, and tools cancel
# defensively (dagster-slurm cancels a job it has just seen fail).
for gone_id in "$id" 99999999; do
  out="$(gridware "scancel '$gone_id' 2>&1; echo rc=\$?")"
  assert_eq "$out" "rc=0" "scancel of job $gone_id, which is not there, exits 0 silently"
done

out="$(gridware "scancel --signal=KILL 99999999 2>&1; echo rc=\$?")"
assert_eq "$out" "rc=0" "scancel --signal=KILL of a job that is not there exits 0 silently"

# The control: a refusal that is NOT "the job is gone" must still fail, with
# the reason, which Grid Engine prints on stdout. Suspending a job that does
# not exist is one a plain user can trigger.
out="$(gridware "scancel --signal=STOP 99999999 2>&1; echo rc=\$?")"
assert_contains "$out" "scancel: error:" "a refused suspend names its reason"
case "$out" in
  *rc=0) fail "a refused suspend exits 0: $out" ;;
  *)     pass "a refused suspend exits non-zero" ;;
esac

# A user with no jobs: qdel -u refuses, SLURM's scancel -u does not.
gridware "scancel --me >/dev/null 2>&1; qdel -u $JOB_USER >/dev/null 2>&1; true"
for _ in $(seq 1 20); do
  gridware "squeue -h -u $JOB_USER 2>/dev/null | grep -q ." || break
  sleep 2
done
out="$(gridware "scancel -u $JOB_USER 2>&1; echo rc=\$?")"
assert_eq "$out" "rc=0" "scancel -u for a user with no jobs exits 0 silently"

# Not a job id: refused before qdel sees it. qdel would read it as a job name,
# answer "does not exist", and the typo would pass for a cancelled job.
out="$(gridware "scancel 47l1 2>&1; echo rc=\$?")"
assert_contains "$out" "Invalid job id 47l1" "scancel names an argument that is not a job id"
assert_contains "$out" "rc=1" "scancel of an argument that is not a job id fails"
finish
