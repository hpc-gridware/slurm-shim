#!/usr/bin/env bash
# Check: stopping a step never deletes the whole job.
#
# Under tight integration each remote stepper is a Grid Engine pe task, and a pe
# task that dies BY a signal makes qmaster delete the entire job (qacct `failed
# 100 : assumedly after job`), killing whatever the batch script does after srun.
# Two paths used to do exactly that: kill-on-bad-exit escalating by SIGKILLing
# the local `qrsh -inherit` clients, and a SIGTERM delivered to a stepper (which
# had no handler). Both must now end the stepper with an exit code, and the
# batch script must run to its end. See docs/solutions/integration-issues/
# pe-task-signal-death-deletes-job.md.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "41_kill_escalation: a stopped step leaves the job and its script alive"

if [ "${EXEC_NODES:-0}" -lt 2 ]; then
  skip "needs 2 exec nodes for a remote stepper (cluster has ${EXEC_NODES:-0})"
  finish
fi

job="$(mktemp)"
ids=""
# Remove the job script, and never leave a job holding nodes for the next check.
trap 'rm -f "$job"; for j in $ids; do gridware "qdel $j" >/dev/null 2>&1 || true; done' EXIT

# ------------------------------------------- kill-on-bad-exit escalation to SIGKILL
# Rank 0 fails; the remote rank ignores SIGTERM, so srun has to escalate. Both
# cases sleep 20s after srun before printing SCRIPT_END: qmaster's job kill lands
# a few seconds after the pe task dies, and a short tail outruns it and passes
# against the bug.
cat >"$job" <<'EOF'
#!/bin/bash
srun bash -c 'if [ "$SLURM_PROCID" = 0 ]; then sleep 2; exit 1; fi; trap "" TERM; exec sleep 120'
echo "SRUN_RC=$?"
sleep 20
echo SCRIPT_END
EOF
remote=$JOB_HOME/e2e-41-escalate.sh
out=$JOB_HOME/e2e-41-escalate.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out" --nodes=2 --ntasks-per-node=1)"
ids="$ids $id"
if [ -n "$id" ]; then
  res="$(jobout "$id" "$out")"
  assert_contains "$res" "SRUN_RC=1" "srun returns the failing rank's code after escalating"
  assert_contains "$res" "SCRIPT_END" "the batch script runs to its end after the escalation"
  assert_eq "$(failed_field "$id")" "0" "qacct failed is 0, not 100: the job was not deleted"
else
  fail "sbatch returned no job id (escalation case)"
fi

# ----------------------------------------------------- SIGTERM to a remote stepper
# Each rank drops a marker in the (shared) home once it runs: the SIGTERM must
# land while the ranks run. Sent earlier, it hits a stepper still waiting for its
# StepSpec, which exits 143 at once without a rank to report (srun then
# synthesizes 1) -- correct, but not the case under test.
cat >"$job" <<'EOF'
#!/bin/bash
srun bash -c 'touch "$HOME/e2e-41-sigterm.up.$SLURM_PROCID"; exec sleep 60'
echo "SRUN_RC=$?"
sleep 20
echo SCRIPT_END
EOF
remote=$JOB_HOME/e2e-41-sigterm.sh
out=$JOB_HOME/e2e-41-sigterm.out
put_job "$job" "$remote"
gridware "rm -f $JOB_HOME/e2e-41-sigterm.up.*"
id="$(sbatch_submit "$remote" "$out" --nodes=2 --ntasks-per-node=1)"
ids="$ids $id"
if [ -n "$id" ]; then
  for _ in $(seq 1 60); do
    [ "$(gridware "ls $JOB_HOME/e2e-41-sigterm.up.* 2>/dev/null | wc -l" || true)" -ge 2 ] && break
    sleep 2
  done
  # Find this job's REMOTE stepper -- a pe task, started by qrsh -inherit. The
  # stepper on srun's own host is srun's child and not a pe task: signalling it
  # would pass against the bug, so skip any stepper whose parent is srun. This
  # does not assume which host srun runs on (a manager may also be an exec host).
  # Its envelope names the job. `|| true`: the harness runs under set -e, and the
  # lookup fails until the stepper has started.
  target="" pid=""
  for _ in $(seq 1 45); do
    for n in "${NODES[@]}"; do
      pid="$(node_sh "$n" "for p in \$(pgrep -f 'slurm-shim stepper --envelope'); do
          pp=\$(awk '{print \$4}' /proc/\$p/stat)
          tr '\0' ' ' < /proc/\$pp/cmdline | grep -Eq '(^|/)srun ' && continue
          env=\$(tr '\0' ' ' < /proc/\$p/cmdline | awk '{print \$NF}')
          echo \"\$env\" | base64 -d 2>/dev/null | grep -q '\"job_id\":$id,' && echo \$p
        done" | head -1 || true)"
      [ -n "$pid" ] && { target="$n"; break 2; }
    done
    sleep 2
  done
  if [ -n "$pid" ]; then
    node_sh "$target" "kill -TERM $pid" || fail "stepper $pid on $target vanished before SIGTERM"
    res="$(jobout "$id" "$out")"
    # The stepper stops its rank with SIGTERM and reports 128+15; kill-on-bad-exit
    # then ends the local rank the same way.
    assert_contains "$res" "SRUN_RC=143" "srun reports the rank the stepper stopped as 143, not success"
    assert_contains "$res" "SCRIPT_END" "the batch script runs to its end after a stepper got SIGTERM"
    assert_eq "$(failed_field "$id")" "0" "qacct failed is 0, not 100: the stepper exited with a code"
  else
    fail "no remote stepper for job $id appeared"
  fi
else
  fail "sbatch returned no job id (SIGTERM case)"
fi

# ------------------------------------------- a process-group signal around srun
# coreutils `timeout` makes itself a process-group leader and signals the whole
# group on expiry. SIGKILL is the discriminating case (a group SIGTERM is also
# survived via the stepper's trap, since qrsh forwards it): a qrsh -inherit
# client killed in that group drops its connection and the remote shepherd
# SIGKILLs the pe task. The rank ignores SIGTERM so the orphaned stepper outlives
# its client by killWait; with a quick rank it exits first and hides the bug. In
# its own process group the client survives srun's death, and the orphaned
# stepper exits with a code.
cat >"$job" <<'EOF'
#!/bin/bash
timeout -s KILL 10 srun bash -c 'trap "" TERM; exec sleep 60'
echo "TIMEOUT_RC=$?"
sleep 20
echo SCRIPT_END
EOF
remote=$JOB_HOME/e2e-41-pgroup.sh
out=$JOB_HOME/e2e-41-pgroup.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out" --nodes=2 --ntasks-per-node=1)"
ids="$ids $id"
if [ -n "$id" ]; then
  res="$(jobout "$id" "$out")"
  assert_contains "$res" "TIMEOUT_RC=137" "timeout SIGKILLed srun's process group"
  assert_contains "$res" "SCRIPT_END" "the batch script runs to its end after a process-group SIGKILL"
  assert_eq "$(failed_field "$id")" "0" "qacct failed is 0, not 100: no qrsh client was signalled"
else
  fail "sbatch returned no job id (process-group case)"
fi

# ------------------------------------------------- Ctrl-C, during and after launch
# A terminal Ctrl-C is a SIGINT to the foreground process group. setsid gives srun
# its own group, so `kill -INT -- -pid` reaches it exactly as a keyboard would.
# - EARLY lands in the launch window (steppers starting, not yet connected). It
#   used to be lost: srun forwarded it before the ranks existed and the step ran
#   to the end (exit 0 after 60s). Now the launch aborts with 130.
# - LATE lands while the ranks run: srun forwards SIGINT and returns 130. 143
#   would mean the master's stepper took the SIGINT itself (back in srun's group).
cat >"$job" <<'EOF'
#!/bin/bash
ctrlc() { # $1 = delay before the Ctrl-C, $2 = label
  local t0=$SECONDS
  setsid srun bash -c 'exec sleep 60' &
  local pid=$!
  sleep "$1"; kill -INT -- -"$pid"
  wait "$pid"; echo "SRUN_RC_$2=$? SECS_$2=$((SECONDS - t0))"
}
ctrlc 0.8 EARLY
ctrlc 6 LATE
sleep 20
echo SCRIPT_END
EOF
remote=$JOB_HOME/e2e-41-ctrlc.sh
out=$JOB_HOME/e2e-41-ctrlc.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out" --nodes=2 --ntasks-per-node=1)"
ids="$ids $id"
if [ -n "$id" ]; then
  res="$(jobout "$id" "$out")"
  assert_contains "$res" "SRUN_RC_EARLY=130" "a Ctrl-C during the launch aborts the step with 130"
  early_secs="$(printf '%s\n' "$res" | sed -n 's/.*SECS_EARLY=\([0-9]*\).*/\1/p')"
  if [ -n "$early_secs" ] && [ "$early_secs" -lt 8 ]; then
    pass "the step stopped promptly (${early_secs}s), not after its ranks finished"
  else
    fail "the early Ctrl-C was lost: the step ran ${early_secs:-?}s"
  fi
  assert_contains "$res" "SRUN_RC_LATE=130" "a Ctrl-C while ranks run is forwarded as SIGINT (130, not 143)"
  assert_contains "$res" "SCRIPT_END" "the batch script runs to its end after both Ctrl-Cs"
  assert_eq "$(failed_field "$id")" "0" "qacct failed is 0, not 100"
else
  fail "sbatch returned no job id (Ctrl-C case)"
fi

finish
