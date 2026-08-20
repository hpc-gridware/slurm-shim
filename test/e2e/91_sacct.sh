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
ok_id="$(gridware "cd /home/gridware && sbatch --job-name=e2eok --wrap='sleep 2'" \
  | awk '/Submitted batch job/{print $NF}')"
[ -n "$ok_id" ] || fail "sbatch returned no job id"

# ------------------------------------------------------------- a failing job
# Dies by signal, which is the failure GE actually records for a PE job: OCS
# 9.1.4 drops a parallel job's clean-exit status, so `exit 3` would come back as
# a success. See docs/solutions/integration-issues/
# pe-jobs-lose-exit-status-in-accounting.md -- the exit-code mapping itself is
# covered by the unit tests in internal/gedata.
bad_id="$(gridware "cd /home/gridware && sbatch --job-name=e2ebad --wrap='kill -TERM \$\$'" \
  | awk '/Submitted batch job/{print $NF}')"
[ -n "$bad_id" ] || fail "sbatch returned no job id for the failing job"

# ------------------------------------------------------- a long job, still live
live_id="$(gridware "cd /home/gridware && sbatch --job-name=e2elive --wrap='sleep 45'" \
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

waitgone "$ok_id" || fail "job $ok_id never left the queue"
waitgone "$bad_id" || fail "job $bad_id never left the queue"
waitacct "$ok_id" || fail "job $ok_id never reached the accounting file"
waitacct "$bad_id" || fail "job $bad_id never reached the accounting file"

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

# ------------------------------------------------------------ parsable modes
p2="$(gridware "sacct -n -j '$ok_id' -o JobID,State --parsable2")"
assert_eq "$p2" "$ok_id|COMPLETED" "--parsable2 has no trailing delimiter"
pp="$(gridware "sacct -n -j '$ok_id' -o JobID,State -P")"
assert_eq "$pp" "$ok_id|COMPLETED|" "-P keeps the trailing delimiter"

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

gridware "qdel '$live_id'" >/dev/null 2>&1
finish
