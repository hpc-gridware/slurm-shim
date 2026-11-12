#!/usr/bin/env bash
# Check: sacct reporting against real qstat/qacct data. Pins the --format
# contract (default columns, every supported field, ExitCode as code:signal,
# -P vs --parsable2) AND the exact machine-readable shape submitit and Hydra
# poll, so a format change cannot silently break them.
# Costs nothing to run: three short jobs, pure shell, no downloads.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "91_sacct: --format fields, ExitCode, parsable modes, -u/-S window"

# waitgone <jobid> blocks (bounded) until the job leaves the queue.
waitgone() {
  for _ in $(seq 1 90); do
    gridware "qstat 2>/dev/null | awk '{print \$1}' | grep -qx '$1'" || return 0
    sleep 2
  done
  return 1
}

# waitacct <jobid> blocks until the job reaches the accounting file. qacct lags
# the queue by a spool flush, so polling qstat alone is not enough.
waitacct() {
  for _ in $(seq 1 45); do
    gridware "qacct -j '$1' >/dev/null 2>&1" && return 0
    sleep 2
  done
  return 1
}

# field <sacct-parsable2-output> <column-name> prints that column of row 1.
field() {
  printf '%s\n' "$1" | awk -F'|' -v want="$2" '
    NR==1 { for (i=1;i<=NF;i++) if ($i==want) col=i; next }
    NR==2 { print (col ? $col : "") }'
}

# ---------------------------------------------------------------- a clean job
ok_id="$(gridware "cd $JOB_HOME && sbatch --job-name=e2eok --wrap='sleep 2'" \
  | awk '/Submitted batch job/{print $NF}')"
[ -n "$ok_id" ] || fail "sbatch returned no job id"

# ------------------------------------------------------------- a failing job
# Dies by signal, which GE records in its `failed` field on every supported
# version.
bad_id="$(gridware "cd $JOB_HOME && sbatch --job-name=e2ebad --wrap='kill -TERM \$\$'" \
  | awk '/Submitted batch job/{print $NF}')"
[ -n "$bad_id" ] || fail "sbatch returned no job id for the failing job"

# ------------------------------------------------- a job that exits non-zero
# The other failure shape, and the one that used to be invisible: through OCS
# 9.1.4 a PE with control_slaves TRUE (required for tight integration, so every
# shim PE has it) dropped the script's clean-exit status and this came back
# COMPLETED/0:0. Fixed in 9.1.5, so the assertion below is version-gated -- the
# shim's mapping never changed. See docs/solutions/integration-issues/
# pe-jobs-lose-exit-status-in-accounting.md.
e3_id="$(gridware "cd $JOB_HOME && sbatch --job-name=e2eexit3 --wrap='exit 3'" \
  | awk '/Submitted batch job/{print $NF}')"
[ -n "$e3_id" ] || fail "sbatch returned no job id for the exit-3 job"

# ------------------------------------------------------------- a 2-node job
# Two tasks on two nodes: its node count (2) differs from its CPU count per node,
# so a column that only happens to be 1 cannot stand in for AllocNodes.
n2_id="$(gridware "cd $JOB_HOME && sbatch --job-name=e2en2 -p batch -N 2 --ntasks-per-node=1 --wrap='sleep 1'" \
  | awk '/Submitted batch job/{print $NF}')"
[ -n "$n2_id" ] || fail "sbatch returned no job id for the 2-node job"

# ------------------------------------------------------- a long job, still live
live_id="$(gridware "cd $JOB_HOME && sbatch --job-name=e2elive --wrap='sleep 45'" \
  | awk '/Submitted batch job/{print $NF}')"
sleep 8

# A running job must report RUNNING with a real node and a start time, and its
# Elapsed must be counting -- not the 00:00:00 of a job with no usage record yet.
live="$(gridware "sacct -j '$live_id' -o JobID,State,NodeList,Start,Elapsed --parsable2")"
assert_eq "$(field "$live" State)" "RUNNING" "a live job reports RUNNING"
if [ -n "$(field "$live" NodeList)" ]; then
  pass "a live job reports the node it landed on"
else
  fail "live job $live_id reported an empty NodeList"
fi
case "$(field "$live" Start)" in
  [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9])
    pass "a live job reports its qstat start time" ;;
  *) fail "live Start is not a SLURM timestamp: '$(field "$live" Start)'" ;;
esac
case "$(field "$live" Elapsed)" in
  00:00:0[0-9]|00:00:1[0-9]) pass "a live job's Elapsed counts from its start time" ;;
  *) fail "live Elapsed should be a few seconds, got '$(field "$live" Elapsed)'" ;;
esac

for id in "$ok_id" "$bad_id" "$e3_id" "$n2_id"; do
  waitgone "$id" || fail "job $id never left the queue"
  waitacct "$id" || fail "job $id never reached the accounting file"
done

# ------------------------------------------------------------- default format
# Without -o, sacct must print SLURM's own default columns. A bare "sacct -j N"
# is what a human types first; it has to be readable.
def="$(gridware "sacct -j '$ok_id'")"
head="$(printf '%s\n' "$def" | sed -n 1p)"
for col in JobID JobName Partition Account AllocCPUS State ExitCode; do
  assert_contains "$head" "$col" "default format includes $col"
done
assert_contains "$(printf '%s\n' "$def" | sed -n 2p)" "---" "the table has a dashed rule"
assert_contains "$(printf '%s\n' "$def" | sed -n 3p)" "e2eok" "the row carries the job name"

# ------------------------------------------------------------- explicit fields
got="$(gridware "sacct -j '$ok_id' -o JobID,JobName,State,ExitCode,User,Partition,AllocCPUS,NodeList,Elapsed,Start,End,Submit,MaxRSS,TotalCPU --parsable2")"
assert_eq "$(field "$got" JobID)" "$ok_id" "JobID is the submitted id"
assert_eq "$(field "$got" JobName)" "e2eok" "JobName comes from --job-name"
assert_eq "$(field "$got" State)" "COMPLETED" "a clean job is COMPLETED"
assert_eq "$(field "$got" ExitCode)" "0:0" "a clean job exits 0:0"
assert_eq "$(field "$got" User)" "gridware" "User is the submitting user"
assert_eq "$(field "$got" AllocCPUS)" "1" "AllocCPUS is the granted slot count"
# AllocNodes is SLURM's other name for the node count; dagster-slurm derives
# node hours from it.
# Literals, not a comparison with NNodes: two empty answers would agree.
assert_eq "$(gridware "sacct -n -P -j '$ok_id' -o AllocNodes")" "1" "AllocNodes is 1 for a serial job"
# A finished job's node count is read from the -par request in its record. OCS
# 9.0 has no -par, so the record of a multi-node job there does not say.
if version_ge "$(ocs_version)" 9.1.5; then
  assert_eq "$(gridware "sacct -n -P -j '$n2_id' -o AllocNodes")" "2" "AllocNodes is 2 for a 2-node job"
  assert_eq "$(gridware "sacct -n -P -j '$n2_id' -o NNodes,AllocNodes")" "2|2" "AllocNodes and NNodes agree"
else
  skip "the node count of a finished multi-node job needs -par (OCS 9.1.5)"
fi
for f in Partition NodeList MaxRSS; do
  if [ -n "$(field "$got" "$f")" ]; then
    pass "$f is populated from the accounting record"
  else
    fail "$f came back empty for finished job $ok_id"
  fi
done
for f in Elapsed TotalCPU; do
  case "$(field "$got" "$f")" in
    [0-9][0-9]:[0-9][0-9]:[0-9][0-9]) pass "$f is HH:MM:SS" ;;
    *) fail "$f is not a duration: '$(field "$got" "$f")'" ;;
  esac
done
for f in Start End Submit; do
  case "$(field "$got" "$f")" in
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9])
      pass "$f is a SLURM timestamp" ;;
    *) fail "$f is not a SLURM timestamp: '$(field "$got" "$f")'" ;;
  esac
done

# The reported times must be the cluster's own wall clock, not shifted into
# another zone: Start has to fall on the cluster's current date.
today="$(gridware 'date +%Y-%m-%d')"
assert_contains "$(field "$got" Start)" "$today" "Start is in the cluster's own timezone"

# --------------------------------------------------------------- exit codes
# A job that died must not be reported as a clean finish: that is what makes
# sacct usable as a completion signal at all.
bad="$(gridware "sacct -j '$bad_id' -o State,ExitCode --parsable2")"
assert_eq "$(field "$bad" State)" "CANCELLED" "a killed job is not COMPLETED"
assert_eq "$(field "$bad" ExitCode)" "0:9" "ExitCode reports the kill as a signal"

# A job that exits non-zero of its own accord. Gated on the OCS the cluster is
# actually running, not on $OCS_VERSION, which is only what was requested.
ocs="$(ocs_version)"
if version_ge "${ocs:-0}" 9.1.5; then
  e3="$(gridware "sacct -j '$e3_id' -o State,ExitCode --parsable2")"
  assert_eq "$(field "$e3" State)" "FAILED" "a job that exits non-zero is FAILED"
  assert_eq "$(field "$e3" ExitCode)" "3:0" "ExitCode carries the script's own exit status"
else
  skip "exit status under control_slaves TRUE is lost on OCS ${ocs:-unknown} (fixed in 9.1.5)"
fi

# ------------------------------------------------------------ parsable modes
p2="$(gridware "sacct -n -j '$ok_id' -o JobID,State --parsable2")"
assert_eq "$p2" "$ok_id|COMPLETED" "--parsable2 has no trailing delimiter"
pp="$(gridware "sacct -n -j '$ok_id' -o JobID,State -P")"
assert_eq "$pp" "$ok_id|COMPLETED" "-P is --parsable2: no trailing delimiter"
pp="$(gridware "sacct -n -j '$ok_id' -o JobID,State -p")"
assert_eq "$pp" "$ok_id|COMPLETED|" "-p keeps the trailing delimiter"

# -X is accepted and changes nothing: there are no step rows to filter out.
xx="$(gridware "sacct -n -X -j '$ok_id' -o JobID,State --parsable2")"
assert_eq "$xx" "$p2" "-X is accepted as a no-op"
assert_eq "$(printf '%s\n' "$p2" | wc -l | tr -d ' ')" "1" \
  "a job yields exactly one row (no .batch/.extern steps)"

# ------------------------------------------------- the submitit/Hydra contract
# This exact invocation is what submitit's watcher runs. Its header and row
# shape are load-bearing; changing them breaks every submitit-based launcher.
sub="$(gridware "sacct -o JobID,State,NodeList --parsable2 -j '$ok_id'")"
assert_eq "$(printf '%s\n' "$sub" | sed -n 1p)" "JobID|State|NodeList" \
  "the submitit query still returns its exact header"
assert_contains "$sub" "$ok_id|COMPLETED|" "the submitit query still returns id|state|node"

# --------------------------------------------------------- -u and -S selection
# -u must reach qacct as a per-job listing: "qacct -o <user>" on its own prints
# an aggregate usage summary, which would yield no rows at all here.
win="$(gridware "sacct -n -u gridware -S ${today}T00:00:00 -o JobID,State --parsable2")"
assert_contains "$win" "$ok_id|COMPLETED" "-u/-S reports the finished job"
assert_contains "$win" "$bad_id|CANCELLED" "-u/-S reports the killed job"
assert_contains "$win" "$live_id|RUNNING" "-u/-S includes still-running jobs"

# A window that ended before these jobs ran must exclude them -- the RUNNING job
# too. qstat answers for right now, so the bounds have to reach the live rows and
# not only qacct.
past="$(gridware "sacct -n -u gridware -S 2000-01-01 -E 2000-01-02 -o JobID --parsable2")"
if printf '%s\n' "$past" | grep -q "$ok_id"; then
  fail "-E did not bound the window: $ok_id showed up in year 2000"
else
  pass "-S/-E bounds the window for finished jobs"
fi
if printf '%s\n' "$past" | grep -q "$live_id"; then
  fail "-E did not bound the window: running job $live_id leaked into year 2000"
else
  pass "-S/-E bounds the window for running jobs too"
fi

# -s selects by state. Silently ignoring it would make the standard
# `sacct -s R -o JobID | xargs scancel` idiom cancel finished jobs as well.
runonly="$(gridware "sacct -n -u gridware -S ${today}T00:00:00 -s R -o JobID,State --parsable2")"
assert_contains "$runonly" "$live_id|RUNNING" "-s R keeps the running job"
if printf '%s\n' "$runonly" | grep -q "$ok_id"; then
  fail "-s R returned the finished job $ok_id"
else
  pass "-s R excludes finished jobs"
fi
donly="$(gridware "sacct -n -u gridware -S ${today}T00:00:00 -s CD -o JobID,State --parsable2")"
assert_contains "$donly" "$ok_id|COMPLETED" "-s CD keeps the completed job"
if printf '%s\n' "$donly" | grep -q "$live_id"; then
  fail "-s CD returned the running job $live_id"
else
  pass "-s CD excludes running jobs"
fi

# An unparsable -S must fail loudly, not silently widen the query to the whole
# accounting file (or report an empty window it never applied).
if gridware "sacct -u gridware -S not-a-time -o JobID >/dev/null 2>&1"; then
  fail "an unparsable -S was accepted silently"
else
  pass "an unparsable -S is rejected"
fi

# A comma-separated -u is valid SLURM and must not degrade to an empty report.
multi="$(gridware "sacct -n -u gridware,nobody -S ${today}T00:00:00 -o JobID --parsable2")"
assert_contains "$multi" "$ok_id" "a comma-separated -u still reports the user's jobs"

# Another user's jobs must not leak into a -u report.
other="$(gridware "sacct -n -u nobody -S ${today}T00:00:00 -o JobID --parsable2")"
if [ -n "$(printf '%s' "$other" | tr -d '[:space:]')" ]; then
  fail "-u nobody returned rows: $other"
else
  pass "-u scopes the report to that user"
fi

# ------------------------------------------------------------------- aliases
al="$(gridware "sacct -n -j '$ok_id' -o JobIDRaw,NCPUS,Nodes --parsable2")"
assert_eq "$al" "$(gridware "sacct -n -j '$ok_id' -o JobID,AllocCPUS,NodeList --parsable2")" \
  "field aliases resolve to their canonical fields"

# ------------------------------------------------- a job that is being removed
# While GE removes a job (qstat state dr) it does not say why, so sacct must not
# name a final state before the accounting record does: a poller keeps the first
# terminal state it sees. And never COMPLETING, which dagster-slurm reads as
# success. Poll through the removal and keep every distinct answer.
del_id="$(gridware "cd $JOB_HOME && sbatch --job-name=e2edel --wrap='sleep 300'" \
  | awk '/Submitted batch job/{print $NF}')"
for _ in $(seq 1 60); do
  [ "$(gridware "squeue -h -j '$del_id' -o %T")" = "RUNNING" ] && break
  sleep 1
done
seen="$(gridware "scancel '$del_id'; last=
  for i in \$(seq 1 300); do
    s=\$(sacct -X -n -P -j '$del_id' -o State)
    if [ -n \"\$s\" ] && [ \"\$s\" != \"\$last\" ]; then printf '%s ' \"\$s\"; last=\$s; fi
    [ \"\$s\" = CANCELLED ] && break
    sleep 0.1
  done")"
case "$(printf '%s' "$seen" | xargs)" in
  "RUNNING CANCELLED" | "CANCELLED")
    pass "a job being removed reads RUNNING until its record says CANCELLED ($(printf '%s' "$seen" | xargs))" ;;
  *) fail "states seen while job $del_id was removed: '$seen'" ;;
esac

# ------------------------------------------ a multi-node job at its time limit
# At the limit GE kills the job script or its tasks first, a race. When the
# tasks go first the script leaves by itself and the record says failed=0 with
# its exit status (CS-3087). Either way the job ran into its limit.
to_id="$(gridware "cd $JOB_HOME && sbatch --job-name=e2eto -p batch -N 2 --ntasks-per-node=1 -t 0:10 -o /dev/null --wrap='srun sleep 300'" \
  | awk '/Submitted batch job/{print $NF}')"
# While it runs, its node count comes from the granted hosts (qstat -j): the
# plain listing names the master queue instance only.
for _ in $(seq 1 30); do
  [ "$(gridware "squeue -h -j '$to_id' -o %T")" = "RUNNING" ] && break
  sleep 0.5
done
assert_eq "$(gridware "sacct -P -j '$to_id' -o NNodes,AllocNodes" | xargs)" "NNodes|AllocNodes 2|2" \
  "a running multi-node job reports its node count, each column under its own title"
to=""
for _ in $(seq 1 90); do
  to="$(gridware "sacct -X -n -P -j '$to_id' -o State,Timelimit")"
  case "$to" in ""|RUNNING*|PENDING*) sleep 1 ;; *) break ;; esac
done
# Before 9.1.5 a parallel job's exit status is lost, so the record reads as a
# clean exit and nothing tells it from a job that finished in time.
if version_ge "${ocs:-0}" 9.1.5; then
  assert_eq "$to" "TIMEOUT|00:00:10" "a multi-node job that runs into its limit is TIMEOUT"
else
  skip "a multi-node time limit reads COMPLETED on OCS ${ocs:-unknown} (exit status lost before 9.1.5)"
fi

gridware "qdel '$live_id'" >/dev/null 2>&1
finish
