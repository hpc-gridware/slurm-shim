#!/usr/bin/env bash
# Check: the resource flags sbatch translates actually reach Grid Engine and take
# effect there.
#
# Why this exists separately from the unit tests: internal/cli/sbatch asserts the
# generated qsub argv against a FAKE runner. That proves we emit `-l h_rt=20`; it
# proves nothing about whether the queue permits h_rt, whether the memory complex
# is satisfiable, or whether `-l gpu=1` actually yields an RSMAP grant. Every
# assertion here reads back from a real qsub.
#
# hard_resource_list is only exposed while a job is live -- the accounting record
# does not keep it in a parseable form -- so each assertion inspects the job while
# it is pending or running, then cleans it up.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "31_sbatch_resources: --time/--mem/--gres/--dependency/%p/--signal reach GE"

CLEANUP_IDS=""
cleanup() { [ -n "$CLEANUP_IDS" ] && gridware "qdel $CLEANUP_IDS >/dev/null 2>&1" || true; }
trap cleanup EXIT

# submit <sbatch args...> echoes the job id (empty when the submit failed).
# The `|| true` is load-bearing: e2e-lib.sh sources cluster/lib.sh, which sets
# -e, so a failing sbatch inside a command substitution would abort the whole
# check -- skipping every later assertion and printing no FAIL line at all.
submit() { gridware "cd && sbatch $*" 2>/dev/null | awk '/Submitted batch job/{print $NF}' || true; }

# jattr <jobid> <qstat -j field> prints that field's value, whitespace stripped.
jattr() {
  gridware "qstat -j '$1' 2>/dev/null | awk -F: '/^$2/{sub(/^[^:]*:/,\"\"); print; exit}'" \
    | tr -d '[:space:]'
}

# jstate <jobid> prints the GE state letters from qstat (empty once it is gone).
jstate() { gridware "qstat 2>/dev/null | awk '\$1==$1 {print \$5; exit}'"; }

# ---------------------------------------------------------------- --time (slow)
# Submitted first and collected last: it is the only assertion that needs a real
# wallclock expiry, so everything else runs while it counts down.
TIME_LIMIT=20
time_id="$(submit "--time=00:00:$TIME_LIMIT --wrap='sleep 300'")"
if [ -n "$time_id" ]; then
  CLEANUP_IDS="$CLEANUP_IDS $time_id"
  pass "--time job submitted (id $time_id)"
  assert_contains "$(jattr "$time_id" hard_resource_list)" "h_rt=$TIME_LIMIT" \
    "--time reaches GE as h_rt"
else
  fail "sbatch --time returned no job id"
fi

# -------------------------------- --mem / --dependency / --array %p / --signal
# All four are request-shape assertions, so they ride on ONE job parked behind a
# hold. That is not just fewer round trips: a held job's qstat -j window never
# closes, where a running job's does, and the --mem request MUST never dispatch.
#
# The hold keeps one qstat -j window open long enough to read all four request
# shapes; a running job's window closes as soon as it finishes.
#
# It used to serve a second purpose that no longer applies: under the old
# memory_complex default (h_vmem) the --mem request was UNSATISFIABLE here (the
# exec hosts define only slots and gpu in complex_values), and an unsatisfiable
# request does not sit harmlessly in qw -- GE tries every host, fails, and marks
# all.q QERROR cluster-wide. The default is now mem_free, which the hosts report
# as a load value, so the request is satisfiable and dispatches normally. Setting
# memory_complex back to h_vmem at this site would reintroduce that hazard.
#
# The blocker still sleeps far longer than this check can run, so the hold cannot
# expire mid-check. Cleanup kills both, so nothing is left sleeping.
#
# Request shape only for --mem: under mem_free the request is a scheduling filter,
# not a limit, so there is no enforcement to assert.
blocker_id="$(submit "--wrap='sleep 3600'")"
held_id=""
if [ -n "$blocker_id" ]; then
  CLEANUP_IDS="$CLEANUP_IDS $blocker_id"
  held_id="$(submit "--dependency=afterany:$blocker_id --mem=100M --array=0-3%2 \
                     --signal=B:USR2@30 --time=00:05:00 --wrap='true'")"
fi
if [ -n "$held_id" ]; then
  CLEANUP_IDS="$CLEANUP_IDS $held_id"
  assert_eq "$(jstate "$held_id")" "hqw" "the held job never dispatched (queue stays clean)"

  res="$(jattr "$held_id" hard_resource_list)"
  assert_contains "$res" "mem_free=100M" "--mem reaches GE as the configured memory complex"
  assert_contains "$res" "h_rt=300" "--time reaches GE as h_rt"
  # The one piece of arithmetic in the translation: s_rt = h_rt - signal lead, which
  # is what delivers submitit's SIGUSR2 early enough to checkpoint.
  assert_contains "$res" "s_rt=270" "--signal lead time becomes the s_rt grace"

  assert_contains "$(jattr "$held_id" jid_predecessor_list)" "$blocker_id" \
    "--dependency reaches GE as -hold_jid"
  assert_eq "$(jattr "$held_id" task_concurrency)" "2" "--array %p reaches GE as -tc"
  assert_contains "$(jattr "$held_id" notify)" "TRUE" "--signal reaches GE as -notify"
  assert_contains "$(jattr "$held_id" restart)" "y" "--signal makes the job rerunnable (-r y)"
else
  fail "could not submit the held job carrying the resource requests"
fi

# ------------------------------------------------------------------------ --gres
# The gap this check was written for: 60_gpu.sh submits with a RAW qsub -l gpu=1,
# so the sbatch --gres -> -l <gres_complex> hop that produces the grant in the
# first place had no live coverage at all. Asserts the whole chain: flag ->
# request -> RSMAP grant -> job env -> step env.
ensure_gpu_complex
gjob="$(mktemp)"
trap 'rm -f "$gjob"; cleanup' EXIT
cat >"$gjob" <<'EOF'
#!/bin/bash
#SBATCH --partition=gpu
#SBATCH --gres=gpu:1
echo "JOBGPUS=[$SLURM_JOB_GPUS] ONNODE=[$SLURM_GPUS_ON_NODE]"
srun bash -c 'echo "rank=$SLURM_PROCID cuda=[$CUDA_VISIBLE_DEVICES]"'
EOF
gremote=/home/gridware/e2e-31-gres.sh
gout=/home/gridware/e2e-31-gres.out
put_job "$gjob" "$gremote"
gridware "rm -f '$gout'"
# Through the DIRECTIVE path (#SBATCH --gres), not the CLI path, so both are covered.
gres_id="$(submit "--output='$gout' '$gremote'")"
if [ -n "$gres_id" ]; then
  # Runs to completion on its own; listed for cleanup only so a job that never
  # gets scheduled cannot outlive the check.
  CLEANUP_IDS="$CLEANUP_IDS $gres_id"
  assert_contains "$(jattr "$gres_id" hard_resource_list)" "${GPU_COMPLEX}=1" \
    "#SBATCH --gres=gpu:1 reaches GE as -l ${GPU_COMPLEX}=1"
  gres_out="$(jobout "$gres_id" "$gout" || true)"
  assert_contains "$gres_out" "JOBGPUS=[0]" "the grant becomes SLURM_JOB_GPUS"
  assert_contains "$gres_out" "ONNODE=[1]" "the grant becomes SLURM_GPUS_ON_NODE"
  assert_contains "$gres_out" "rank=0 cuda=[0]" "the grant reaches the step as CUDA_VISIBLE_DEVICES"
else
  fail "sbatch of the --gres job returned no id"
fi

# ------------------------------------------------------- malformed values (loud)
# REQ-SBT-005: a bad request must fail at submit time, not become a job that never
# runs. One rejection from each layer -- the shim's own parser and GE's.
if gridware "cd && sbatch --time=notatime --wrap=true >/dev/null 2>&1"; then
  fail "sbatch accepted --time=notatime"
else
  pass "a malformed --time is rejected at submit time"
fi
if gridware "cd && sbatch --mem=abc --wrap=true >/dev/null 2>&1"; then
  fail "sbatch accepted --mem=abc"
else
  pass "GE's rejection of a bad memory value is surfaced, not swallowed"
fi

# ------------------------------------------------------ collect the --time job
# Back to the slow one. GE must actually enforce h_rt: the script asked to sleep
# 300s, so a job that ends anywhere near the limit proves the request took effect.
if [ -n "$time_id" ]; then
  for _ in $(seq 1 45); do
    [ -z "$(jstate "$time_id")" ] && break
    sleep 2
  done
  for _ in $(seq 1 30); do
    gridware "qacct -j '$time_id' >/dev/null 2>&1" && break
    sleep 2
  done
  wall="$(gridware "qacct -j '$time_id' 2>/dev/null | awk '/^ru_wallclock/{print \$2; exit}'")"
  if [ -n "$wall" ] && [ "$wall" -le $((TIME_LIMIT + 15)) ]; then
    pass "GE enforced h_rt (job asked for 300s, ran ${wall}s)"
  else
    fail "h_rt was not enforced: ru_wallclock='$wall', expected <= $((TIME_LIMIT + 15))"
  fi
  # NOTE: real SLURM reports TIMEOUT for a --time expiry. GE's execd kills the job
  # itself and records failed=100 (the same code as a qdel), not the failed=37 the
  # qmaster path would give, so the shim reports CANCELLED. That is a genuine
  # fidelity gap, pinned here so a future mapping change is a deliberate one.
  # Assert the GE-side fact directly as well as the shim's mapping of it, so a
  # failure says which half moved: acctState keys only on `failed`.
  gefailed="$(gridware "qacct -j '$time_id' 2>/dev/null | awk '/^failed/{print \$2; exit}'")"
  assert_eq "$gefailed" "100" "GE records an execd-enforced h_rt kill as failed=100"
  st="$(gridware "sacct -n -j '$time_id' -o State,ExitCode --parsable2 | head -1")"
  assert_eq "${st%%|*}" "CANCELLED" "a --time expiry is terminal in sacct (SLURM would say TIMEOUT)"
  assert_eq "${st#*|}" "0:9" "the kill is reported as a signal, not an exit code"
fi

# ------------------------------------------------- --mem must not cap ADDRESS SPACE
# The regression test for the whole bug class: under an address-space enforced
# complex (h_vmem) a --mem job runs with a finite RLIMIT_AS, and a CUDA context --
# which reserves tens of GB of address space at init and touches almost none of it
# -- dies before any user code runs. Measured on OCS 9.1.5: `-l h_vmem=1G` yields
# ulimit -v 1048576 and a 2 GiB allocation fails with "memory exhausted".
#
# No GPU is needed to catch it: the address-space cap either exists or it does not,
# and a plain allocation proves it. Output goes to the shared home, never /tmp,
# which is node-local here (a job on a worker would write where we cannot see it).
memjob="$(mktemp)"
cat >"$memjob" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=1
#SBATCH --mem=100M
echo "ULIMIT_V=$(ulimit -v)"
dd if=/dev/zero of=/dev/null bs=2G count=1 2>&1 | tail -1
EOF
mem_remote=/home/gridware/e2e-31-mem.sh
mem_out=/home/gridware/e2e-31-mem.out
put_job "$memjob" "$mem_remote"
rm -f "$memjob"
mem_id="$(sbatch_submit "$mem_remote" "$mem_out")"
if [ -z "$mem_id" ]; then
  fail "could not submit the address-space check job"
else
  mem_res="$(jobout "$mem_id" "$mem_out")"
  assert_contains "$mem_res" "ULIMIT_V=unlimited" \
    "--mem does not cap virtual address space (a CUDA context would survive)"
  case "$mem_res" in
    *"memory exhausted"*) fail "--mem capped the address space: a 2 GiB allocation was refused" ;;
    *"copied"*) pass "a 2 GiB allocation succeeds under --mem" ;;
    *) fail "address-space check produced no allocation result: $mem_res" ;;
  esac
  gridware "rm -f '$mem_remote' '$mem_out'"
fi

finish
