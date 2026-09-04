#!/usr/bin/env bash
# Check: an UNMODIFIED SLURM batch script gets the fabricated environment through
# the queue's starter_method -- no hook line in the script (REQ-FAB-010).
#
# This is the check behind the README's "existing SLURM batch scripts keep
# working" claim. It also pins the properties that make a starter safe to wire
# cluster-wide: native GE jobs in the same queue are unaffected, a failed
# fabrication fails the JOB (not the queue instance), srun steps survive a
# job-level abort policy (the starter short-circuits its own stepper), and the
# install tree is root-owned.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "06_starter: unmodified SLURM scripts get SLURM_* via starter_method"

STARTER=/opt/slurm-shim/bin/slurm-shim-starter
HOOK=/opt/slurm-shim/etc/slurm-shim-source-hook.sh

wired="$(manager "qconf -sq all.q | awk '/^starter_method/{print \$2}'")"
assert_eq "$wired" "$STARTER" "all.q starter_method is the shim starter"

# The starter runs as the job user for every job in the queue, so nothing it
# executes -- nor any directory on the path to it -- may be writable by that user.
# Checked on EVERY node (the starter runs on all of them), over the binary, the
# hook, the shim binary, and the containing directories (todos/035).
SHIM_BIN=/opt/slurm-shim/bin/slurm-shim
for n in "${NODES[@]}"; do
  for f in "$STARTER" "$HOOK" "$SHIM_BIN" \
           /opt/slurm-shim /opt/slurm-shim/bin /opt/slurm-shim/etc; do
    owner="$(docker exec "$n" stat -c %U "$f" 2>/dev/null)"
    assert_eq "$owner" "root" "$n: $f is root-owned"
    if docker exec "$n" find "$f" -maxdepth 0 \( -perm -0020 -o -perm -0002 \) 2>/dev/null | grep -q .; then
      fail "$n: $f is group- or world-writable"
    else
      pass "$n: $f is not group/world-writable"
    fi
  done
done

job="$(mktemp)"
# Two cluster-global settings are mutated below and must survive any exit path.
fab_orig="$(manager "qconf -sp smp | awk '/^start_proc_args/{print \$2}'")"
mode_orig="$(manager "qconf -sq all.q | awk '/^shell_start_mode/{print \$2}'")"
[ -n "$fab_orig" ] && [ -n "$mode_orig" ] || { fail "could not read smp start_proc_args / all.q shell_start_mode"; finish; }
restore_cluster() {
  manager "qconf -mattr pe start_proc_args '$fab_orig' smp >/dev/null 2>&1"
  manager "qconf -mattr queue shell_start_mode '$mode_orig' all.q >/dev/null 2>&1"
}
trap 'rm -f "$job"; restore_cluster' EXIT
# wait_job <id>: block until the job leaves qstat (bounded).
wait_job() { for _ in $(seq 1 60); do gridware "qstat -j '$1' >/dev/null 2>&1" || return 0; sleep 2; done; }

# (1) A stock SLURM script: #SBATCH directives, srun, scontrol -- and nothing
# shim-specific. This is what a user brings from a real SLURM cluster.
cat >"$job" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=2
echo "NODELIST=$SLURM_JOB_NODELIST"
echo "NNODES=$SLURM_NNODES NTASKS=$SLURM_NTASKS"
echo "MASTER=$(scontrol show hostnames | head -n1) SELF=$(hostname)"
echo "HOSTS=$(scontrol show hostnames | tr '\n' ',')"
srun bash -c 'echo "RANK $SLURM_PROCID on $(hostname)"'
EOF
remote=/home/gridware/e2e-06-pristine.sh
out=/home/gridware/e2e-06-pristine.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out")"
if [ -n "$id" ]; then pass "pristine script accepted (job $id)"; else fail "pristine script did not submit"; fi
res="$(jobout "$id" "$out")"
assert_contains "$res" "NNODES=3 NTASKS=6" "unmodified script sees SLURM_NNODES/NTASKS"
assert_contains "$res" "NODELIST=ocs-" "unmodified script sees SLURM_JOB_NODELIST"
hosts="$(printf '%s\n' "$res" | sed -n 's/^HOSTS=//p')"
n="$(printf '%s' "$hosts" | tr ',' '\n' | grep -c .)"
assert_eq "$n" "3" "scontrol show hostnames expands to 3 hosts with no hook line"
master="$(printf '%s\n' "$res" | sed -n 's/^MASTER=\([^ ]*\) SELF=\(.*\)$/\1 \2/p')"
case "$master" in
  "ocs-master ocs-master"|"ocs-worker1 ocs-worker1"|"ocs-worker2 ocs-worker2") pass "first hostname is the master host ($master)" ;;
  *) fail "first hostname is not the master host: '$master'" ;;
esac
ranks="$(printf '%s\n' "$res" | grep -c '^RANK ')"
assert_eq "$ranks" "6" "srun fans 6 ranks out under the starter"

# (2) A NATIVE Open Cluster Scheduler job in the same queue never ran the fabricator and
# must be unaffected: the hook's default policy is continue.
cat >"$job" <<'EOF'
#!/bin/bash
echo "NATIVE SLURM_JOB_ID=[${SLURM_JOB_ID:-unset}]"
EOF
remote=/home/gridware/e2e-06-native.sh
out=/home/gridware/e2e-06-native.out
put_job "$job" "$remote"
gridware "rm -f '$out'"
nid="$(gridware "qsub -terse -q all.q -o '$out' -j y '$remote'")"
for _ in $(seq 1 60); do gridware "qstat -j '$nid' >/dev/null 2>&1" || break; sleep 2; done
res="$(gridware "cat '$out' 2>/dev/null")"
assert_contains "$res" "NATIVE SLURM_JOB_ID=[unset]" "native qsub job runs with no SLURM_* and no abort"
acct=""
for _ in $(seq 1 15); do
  acct="$(gridware "qacct -j '$nid' 2>/dev/null | awk '/^(failed|exit_status)/{print \$1\"=\"\$2}' | tr '\n' ' '")"
  [ -n "$acct" ] && break; sleep 2
done
assert_contains "$acct" "failed=0" "native job: failed=0"
assert_contains "$acct" "exit_status=0" "native job: exit_status=0"

# (3) The starter passes its own stepper launch through BEFORE consulting the
# hook, so a failed fabrication or an abort policy cannot kill a step. Tested
# directly (not via a scheduled job): srun's qrsh -inherit does not forward the
# job env to the stepper, so a job-level export could never reach it -- the only
# faithful test is to invoke the starter as srun does. Deleting the starter's
# short-circuit makes this fail (the hook would see the sentinel and exit 1).
res="$(gridware 'd=$(mktemp -d); mkdir -p "$d/slurm_shim"; : > "$d/slurm_shim/environment.failed"; printf "#!/bin/sh\necho STEPPER-RAN\n" > "$d/slurm-shim"; chmod +x "$d/slurm-shim"; TMPDIR="$d" SLURM_SHIM_HOOK_MISSING_ENV=abort /opt/slurm-shim/bin/slurm-shim-starter "$d/slurm-shim" stepper --envelope X 2>&1; echo "RC=$?"; rm -rf "$d"')"
assert_contains "$res" "STEPPER-RAN" "the starter passes a stepper launch through before the hook (sentinel + abort policy notwithstanding)"

# (4) A failed fabrication must fail the JOB, not the queue instance. Point the
# single-node 'smp' PE at a stand-in that does exactly what the fabricator does
# on failure (write the sentinel, exit 0), restore it on exit, and check both
# the job's fate and every all.q instance afterwards.
stand_in=/home/gridware/e2e-06-failfab.sh
gridware "printf '%s\n' '#!/bin/sh' 'mkdir -p \"\${TMPDIR:-/tmp}/slurm_shim\" && touch \"\${TMPDIR:-/tmp}/slurm_shim/environment.failed\"' 'exit 0' > '$stand_in'; chmod 755 '$stand_in'"
manager "qconf -mattr pe start_proc_args '$stand_in' smp >/dev/null"
cat >"$job" <<'EOF'
#!/bin/bash
#SBATCH --partition=smp
#SBATCH --nodes=1
echo "BODY RAN"
EOF
remote=/home/gridware/e2e-06-sentinel.sh
out=/home/gridware/e2e-06-sentinel.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out")"
[ -n "$id" ] || { fail "submit produced no job id"; finish; }
res="$(jobout "$id" "$out")"
manager "qconf -mattr pe start_proc_args '$fab_orig' smp >/dev/null"
assert_contains "$res" "aborting job" "failed fabrication: the hook aborts through the starter"
case "$res" in *"BODY RAN"*) fail "failed fabrication: job body ran anyway" ;; *) pass "failed fabrication: job body did not run" ;; esac
acct=""
for _ in $(seq 1 15); do
  acct="$(gridware "qacct -j '$id' 2>/dev/null | awk '/^(failed|exit_status)/{print \$1\"=\"\$2}' | tr '\n' ' '")"
  [ -n "$acct" ] && break; sleep 2
done
assert_contains "$acct" "failed=0" "failed fabrication: GE failed=0 (a job failure, not an infra failure)"
# The exit status the starter returns reaches qacct only on OCS 9.1.5+: under a
# control_slaves TRUE PE, older releases record exit_status 0 for any job (see
# docs/solutions/integration-issues/pe-jobs-lose-exit-status-in-accounting.md).
# Gated on the OCS the cluster is actually running, as 91_sacct does.
ocs="$(ocs_version)"
if version_ge "${ocs:-0}" 9.1.5; then
  assert_contains "$acct" "exit_status=1" "failed fabrication: job exit_status=1 (sacct FAILED, 1:0)"
else
  skip "exit status under control_slaves TRUE is lost on OCS ${ocs:-unknown} (fixed in 9.1.5)"
fi
# Assert the probe actually saw the queue before trusting the E-state result: a
# failed qstat or a renamed queue would otherwise make an empty result read as
# "no E state" (todos/036).
insts="$(gridware "qstat -f 2>/dev/null | grep -c '^all\.q@' || true")"
assert_eq "$insts" "3" "qstat saw all three all.q instances (E-state probe ran)"
bad="$(gridware "qstat -f 2>/dev/null | awk '\$1 ~ /^all\\.q@/ && NF >= 6 && \$6 ~ /E/' | grep -c . || true")"
assert_eq "$bad" "0" "no all.q instance went into E state"

# (5) SLURM runs a shebang-less script under the user's shell; so must the starter.
printf '#SBATCH --partition=batch\necho "NOSHEBANG NODELIST=$SLURM_JOB_NODELIST"\n' >"$job"
remote=/home/gridware/e2e-06-noshebang.sh
out=/home/gridware/e2e-06-noshebang.out
put_job "$job" "$remote"
id="$(sbatch_submit "$remote" "$out")"
[ -n "$id" ] || { fail "submit produced no job id"; finish; }
res="$(jobout "$id" "$out")"
assert_contains "$res" "NOSHEBANG NODELIST=ocs-" "shebang-less script runs and sees the environment"

# (6) The exploit the starter made reachable (todos/029): a co-tenant pre-creates
# the predictable per-job TMPDIR world-writable and plants a failure sentinel in
# it before the job starts. OCS chowns the directory to the job owner but keeps the
# mode. root stands in for the co-tenant. A NATIVE job must still run, and the
# hook must say why it ignored the state.
cat >"$job" <<'EOF'
#!/bin/bash
echo "PLANTED native ran; SLURM_JOB_ID=[${SLURM_JOB_ID:-unset}]"
EOF
remote=/home/gridware/e2e-06-planted.sh
out=/home/gridware/e2e-06-planted.out
put_job "$job" "$remote"; gridware "rm -f '$out'"
pid="$(gridware "qsub -terse -h -q all.q@ocs-master -o '$out' -j y '$remote'")"
if [ -n "$pid" ]; then pass "held native job submitted ($pid)"; else fail "held native job did not submit"; fi
planted="/tmp/$pid.1.all.q"
manager "mkdir -p '$planted/slurm_shim' && chmod 777 '$planted' '$planted/slurm_shim' && : > '$planted/slurm_shim/environment.failed' && chmod 666 '$planted/slurm_shim/environment.failed'"
gridware "qrls '$pid' >/dev/null 2>&1"; wait_job "$pid"
res="$(gridware "cat '$out' 2>/dev/null")"; manager "rm -rf '$planted'"
assert_contains "$res" "PLANTED native ran" "planted sentinel (029): a native job still runs"
assert_contains "$res" "not a private directory" "planted sentinel (029): the hook reported why it ignored the state"

# (7) Same planting against a PE job: the fabricator must reclaim the job's own
# TMPDIR (strip the foreign write bit, move the planted dir aside) and the job
# must get its real environment, not silently run without one.
cat >"$job" <<'EOF'
#!/bin/bash
echo "PLANTED pe NODELIST=[${SLURM_JOB_NODELIST:-unset}]"
EOF
remote=/home/gridware/e2e-06-reclaim.sh
out=/home/gridware/e2e-06-reclaim.out
put_job "$job" "$remote"; gridware "rm -f '$out'"
pid="$(gridware "qsub -terse -h -pe make 2 -q all.q@ocs-master -o '$out' -j y '$remote'")"
planted="/tmp/$pid.1.all.q"
manager "mkdir -p '$planted/slurm_shim' && chmod 777 '$planted' '$planted/slurm_shim' && : > '$planted/slurm_shim/environment.failed'"
gridware "qrls '$pid' >/dev/null 2>&1"; wait_job "$pid"
res="$(gridware "cat '$out' 2>/dev/null")"; manager "rm -rf '$planted'"
assert_contains "$res" "PLANTED pe NODELIST=[ocs-master" "planted sentinel (029): the fabricator reclaimed TMPDIR and the PE job got its environment"

# (8) shell_start_mode posix_compliant (todos/031): OCS would have run the script
# under the -S shell as a LOGIN shell; the starter must do the same. A #!/bin/sh
# shebang plus `shopt` discriminates: exec'ing the script directly (the old
# behaviour) yields a non-login sh.
manager "qconf -mattr queue shell_start_mode posix_compliant all.q >/dev/null"
cat >"$job" <<'EOF'
#!/bin/sh
if shopt -q login_shell 2>/dev/null; then echo "PC LOGIN=yes"; else echo "PC LOGIN=no"; fi
echo "PC BASH=${BASH_VERSION:-none}"
EOF
remote=/home/gridware/e2e-06-posix.sh
out=/home/gridware/e2e-06-posix.out
put_job "$job" "$remote"; gridware "rm -f '$out'"
pid="$(gridware "qsub -terse -S /bin/bash -q all.q@ocs-master -o '$out' -j y '$remote'")"
wait_job "$pid"
res="$(gridware "cat '$out' 2>/dev/null")"
manager "qconf -mattr queue shell_start_mode '$mode_orig' all.q >/dev/null"
assert_contains "$res" "PC LOGIN=yes" "posix_compliant + -S /bin/bash (031): started as a login shell"
case "$res" in
  *"PC BASH=none"*) fail "posix_compliant (031): -S /bin/bash was not honoured (script ran under sh)" ;;
  *"PC BASH="*)     pass "posix_compliant (031): -S /bin/bash honoured" ;;
  *)                fail "posix_compliant (031): no output" ;;
esac

# (9) shell_start_mode script_from_stdin (todos/031): OCS hands the starter `-s`
# with the script on stdin. The old starter died at `exec -s`.
manager "qconf -mattr queue shell_start_mode script_from_stdin all.q >/dev/null"
cat >"$job" <<'EOF'
#!/bin/bash
echo "SFS ran under ${BASH_VERSION:+bash}"
EOF
remote=/home/gridware/e2e-06-stdin.sh
out=/home/gridware/e2e-06-stdin.out
put_job "$job" "$remote"; gridware "rm -f '$out'"
pid="$(gridware "qsub -terse -q all.q@ocs-master -o '$out' -j y '$remote'")"
wait_job "$pid"
res="$(gridware "cat '$out' 2>/dev/null")"
manager "qconf -mattr queue shell_start_mode '$mode_orig' all.q >/dev/null"
assert_contains "$res" "SFS ran under bash" "script_from_stdin (031): the starter runs the script from stdin"

finish
