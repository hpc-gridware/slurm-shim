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
got_time="$(gridware "squeue -h -j '$mid' -o '%M' 2>/dev/null" | tr -d ' ')"
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

gridware "scancel '$id' >/dev/null 2>&1"
gone=""
for _ in $(seq 1 20); do
  gridware "squeue -h -j '$id' 2>/dev/null | grep -q ." || { gone=1; break; }
  sleep 2
done
if [ -n "$gone" ]; then pass "scancel removed the job from the queue"; else fail "job still present after scancel"; fi
gridware "qdel '$id' >/dev/null 2>&1 || true"
finish
