#!/usr/bin/env bash
# Check: the job-control and inspection habits of a SLURM user work against
# Grid Engine -- holds, a delayed start, requeue/mail/nice flags, scontrol
# hold/release/update/suspend/resume/show, the squeue filters and pending
# reasons, sacct's new fields and states, and the SBATCH_* defaults.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "24_control: holds, --begin, scontrol, squeue filters and reasons, sacct"

CLEANUP_IDS=""
cleanup() { [ -n "$CLEANUP_IDS" ] && gridware "qdel $CLEANUP_IDS >/dev/null 2>&1" || true; }
trap cleanup EXIT

sub() { gridware "cd && sbatch --parsable $* </dev/null" 2>/dev/null | tail -1 || true; }
jattr() {
  gridware "qstat -j '$1' 2>/dev/null | awk -F: '/^$2:/{sub(/^[^:]*:/,\"\"); print; exit}'" | tr -d '[:space:]'
}
jstate() { gridware "qstat 2>/dev/null | awk '\$1==$1 {print \$5; exit}'"; }
reason() { gridware "squeue -h -j $1 -o %r 2>/dev/null"; }
wait_state() { # wait_state <id> <state> waits up to 60s
  for _ in $(seq 1 60); do [ "$(jstate "$1")" = "$2" ] && return 0; sleep 1; done
  return 1
}

# -- holds ------------------------------------------------------------------
h="$(sub "-H -J e2e24h --wrap='sleep 30'")"
CLEANUP_IDS="$CLEANUP_IDS $h"
assert_eq "$(jstate "$h")" "hqw" "sbatch -H submits the job held"
assert_eq "$(reason "$h")" "JobHeldUser" "squeue shows a held job as JobHeldUser"
gridware "scontrol release $h"
wait_state "$h" r && pass "scontrol release lets it run" || fail "the released job did not start"
# Grid Engine fixes a job's h_rt when it starts: changing it later would
# report success and still kill the job at the old limit.
res="$(gridware "scontrol update JobId=$h TimeLimit=10 2>&1; echo rc=\$?")"
assert_contains "$res" "has started" "scontrol update TimeLimit refuses a running job"
assert_contains "$res" "rc=1" "and exits 1"
gridware "scontrol suspend $h"
wait_state "$h" s && pass "scontrol suspend suspends a running job" || fail "job $h was not suspended ($(jstate "$h"))"
assert_eq "$(gridware "squeue -h -j $h -o %t")" "S" "squeue shows it as S"
gridware "scontrol resume $h"
wait_state "$h" r && pass "scontrol resume resumes it" || fail "job $h was not resumed ($(jstate "$h"))"

p="$(sub "-J e2e24p --wrap='sleep 30'")"
CLEANUP_IDS="$CLEANUP_IDS $p"
gridware "scontrol hold $p" >/dev/null 2>&1
case "$(jstate "$p")" in *h*) pass "scontrol hold holds a job" ;; *) fail "scontrol hold did not hold job $p ($(jstate "$p"))" ;; esac

# -- delayed start, dependency ----------------------------------------------
b="$(sub "--begin=now+1hour -J e2e24b --wrap=true")"
CLEANUP_IDS="$CLEANUP_IDS $b"
assert_eq "$(reason "$b")" "BeginTime" "--begin in the future shows as BeginTime"
d="$(sub "--dependency=afterany:$p -J e2e24d --wrap=true")"
CLEANUP_IDS="$CLEANUP_IDS $d"
assert_eq "$(reason "$d")" "Dependency" "a dependent job shows as Dependency"

# -- flags that only change the job's attributes -----------------------------
f="$(sub "-H --requeue --mail-type=END,FAIL --mail-user=e2e@example.com --nice=10 -J e2e24f --wrap=true")"
CLEANUP_IDS="$CLEANUP_IDS $f"
assert_eq "$(jattr "$f" restart)" "y" "--requeue makes the job rerunnable"
assert_eq "$(jattr "$f" mail_options)" "ae" "--mail-type=END,FAIL is mail on end and abort"
assert_eq "$(jattr "$f" mail_list)" "e2e@example.com" "--mail-user sets the mail list"
assert_eq "$(jattr "$f" priority)" "-10" "--nice=10 lowers the priority by 10"

# -- scontrol update / show --------------------------------------------------
u="$(sub "-H -t 10 --gres=gpu:0 -J e2e24u --wrap=true")"
CLEANUP_IDS="$CLEANUP_IDS $u"
gridware "scontrol update JobId=$u TimeLimit=30 JobName=e2e24renamed"
assert_contains "$(jattr "$u" hard_resource_list)" "h_rt=1800" "scontrol update TimeLimit=30 sets h_rt"
assert_eq "$(jattr "$u" job_name)" "e2e24renamed" "scontrol update JobName renames the job"
res="$(gridware "scontrol update JobId=$u Partition=gpu 2>&1; echo rc=\$?")"
assert_contains "$res" "rc=1" "an unsupported field is refused"
rec="$(gridware "scontrol show job $u")"
assert_contains "$rec" "JobState=PENDING" "scontrol show job reports JobState"
assert_contains "$rec" "Reason=JobHeldUser" "and the pending reason"
assert_contains "$rec" "TimeLimit=00:30:00" "and the new time limit"
assert_eq "$(gridware "scontrol show job -o $u" | wc -l | tr -d ' ')" "1" "-o prints one line"
assert_contains "$(gridware "scontrol show node")" "NodeName=" "scontrol show node lists nodes"
assert_contains "$(gridware "scontrol show partition")" "PartitionName=" "scontrol show partition lists partitions"

# -- squeue filters ----------------------------------------------------------
assert_contains "$(gridware "squeue -h --me -t PD -o %i")" "$u" "squeue --me -t PD lists the pending job"
case "$(gridware "squeue -h --me -t R -o %i")" in *"$u"*) fail "squeue -t R listed a pending job" ;; *) pass "squeue -t R leaves pending jobs out" ;; esac
assert_eq "$(gridware "squeue -h -n e2e24renamed -o %j")" "e2e24renamed" "squeue -n filters by name"
assert_contains "$(gridware "squeue -h -j $u,$b -o %i" | tr '\n' ' ')" "$b" "squeue -j takes a list"
assert_contains "$(gridware "squeue -h -j $u -O JobID,Name,TimeLimit")" "30:00" "squeue -O renders TimeLimit"

# -- sacct -------------------------------------------------------------------
t="$(sub "-t 0:05 -J e2e24t --wrap='sleep 120'")"
n="$(sub "-J e2e24name --wrap=true")"
CLEANUP_IDS="$CLEANUP_IDS $t $n"
for _ in $(seq 1 90); do
  [ -z "$(jstate "$t")" ] && [ -z "$(jstate "$n")" ] && break
  sleep 2
done
sleep 3
assert_contains "$(gridware "sacct -n -X -j $t -o State -P")" "TIMEOUT" "a job killed at --time is TIMEOUT"
assert_contains "$(gridware "sacct -n -X --name=e2e24name -j $n -o JobName,State -P")" "e2e24name|COMPLETED" "sacct --name selects by name"
assert_contains "$(gridware "sacct -n -X -j $t -o Timelimit -P")" "00:00:05" "sacct Timelimit comes from the request"
assert_contains "$(gridware "sacct -n -b -j $n -P")" "COMPLETED" "sacct -b prints the brief columns"

# -- SBATCH_* defaults -------------------------------------------------------
e="$(gridware "cd && SBATCH_JOB_NAME=e2e24env SBATCH_TIMELIMIT=7 sbatch --parsable -H --wrap=true </dev/null" | tail -1)"
CLEANUP_IDS="$CLEANUP_IDS $e"
assert_eq "$(jattr "$e" job_name)" "e2e24env" "SBATCH_JOB_NAME names the job"
assert_contains "$(jattr "$e" hard_resource_list)" "h_rt=420" "SBATCH_TIMELIMIT sets the time limit"

finish
