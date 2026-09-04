#!/usr/bin/env bash
# Check: the sourcing hook ENFORCES its abort paths.
#
# Regression guard. The hook is the enforcement point for a failed fabrication:
# the fabricator deliberately exits 0 (a non-zero start_proc_args would put the
# queue instance into an error state), so the sentinel file is the only signal
# that a job must not run. Before this check existed the hook printed "aborting
# job" and returned, which -- sourced bare, as every job script here does -- let
# the job run on with no SLURM_* environment at all.
#
# Runs the installed hook in the master container against a fabricated TMPDIR,
# so it exercises the deployed artifact in the real shell. No scheduling needed.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "05_hook: the source hook enforces abort, and only where it should"

HOOK=/opt/slurm-shim/etc/slurm-shim-source-hook.sh
gridware "test -f $HOOK" || { fail "hook not installed at $HOOK"; finish; }

# hook_case <case-dir-setup> <policy> -- prints "<job-exit> <nodelist-or-unset>".
# The job body runs only if the hook let it; a bare source is used on purpose.
hook_case() {
  local setup="$1" policy="$2"
  gridware "
    d=\$(mktemp -d) && mkdir -p \"\$d/slurm_shim\" && cd \"\$d\" || exit 99
    $setup
    printf '%s\n' 'export TMPDIR=\"'\"\$d\"'\"' \
      ${policy:+'export SLURM_SHIM_HOOK_MISSING_ENV=$policy'} \
      '. $HOOK' \
      'echo \"BODY \${SLURM_JOB_NODELIST:-unset}\"' > job.sh
    bash job.sh 2>&1; echo \"EXIT \$?\"
    rm -rf \"\$d\"
  "
}

# (a) Fabrication failed. Unconditional abort -- no policy knob, and the job body
# must not run. This is the case the whole sentinel design exists for.
res="$(hook_case 'touch "$d/slurm_shim/environment.failed"' '')"
assert_contains "$res" "EXIT 1" "(a) sentinel: job aborts with exit 1"
case "$res" in *BODY*) fail "(a) sentinel: job body ran past a failed fabrication" ;;
               *) pass "(a) sentinel: job body did not run" ;; esac

# (c) No environment file, default policy: continue. A site-wide hook must not
# kill jobs that never ran the fabricator.
res="$(hook_case ':' '')"
assert_contains "$res" "EXIT 0" "(c) no env, default policy: job continues"
assert_contains "$res" "BODY unset" "(c) no env, default policy: body ran without SLURM_*"

# (c) No environment file, policy=abort.
res="$(hook_case ':' 'abort')"
assert_contains "$res" "EXIT 1" "(c) no env, policy=abort: job aborts"
case "$res" in *BODY*) fail "(c) policy=abort: job body ran anyway" ;;
               *) pass "(c) policy=abort: job body did not run" ;; esac

# (b) A good environment file is sourced and the job continues.
res="$(hook_case 'printf "export SLURM_JOB_NODELIST=n[1-2]\n" > "$d/slurm_shim/environment"; chmod 600 "$d/slurm_shim/environment"' '')"
assert_contains "$res" "BODY n[1-2]" "(b) good env: sourced into the job"
assert_contains "$res" "EXIT 0" "(b) good env: job continues"

# (b) A symlink is rejected (co-tenant pre-planting a file in the predictable
# per-job TMPDIR); under policy=abort that rejection must abort, not warn.
res="$(hook_case 'printf "export SLURM_JOB_NODELIST=evil\n" > "$d/planted"; ln -s "$d/planted" "$d/slurm_shim/environment"' 'abort')"
assert_contains "$res" "EXIT 1" "(b) symlink + policy=abort: job aborts"
case "$res" in *evil*) fail "(b) symlink: planted environment was sourced" ;;
               *) pass "(b) symlink: planted environment was not sourced" ;; esac

# ---- Co-tenant cases (todos/029, 030). The per-job TMPDIR is a predictable path
# a co-tenant can pre-create; nothing in it may be believed until it is proven
# the job's own. Each of these would have killed the job (or sourced foreign shell
# code) before the trust checks; now they are reported and ignored.

# (d) The sentinel is a symlink: ignored, job runs.
res="$(hook_case 'printf x > "$d/planted"; ln -s "$d/planted" "$d/slurm_shim/environment.failed"' '')"
assert_contains "$res" "EXIT 0" "(d) symlink sentinel: ignored, job continues"
assert_contains "$res" "BODY" "(d) symlink sentinel: job body ran"

# (e) The sentinel is writable by others: ignored, job runs.
res="$(hook_case 'touch "$d/slurm_shim/environment.failed"; chmod 666 "$d/slurm_shim/environment.failed"' '')"
assert_contains "$res" "BODY" "(e) group/world-writable sentinel: ignored, job body ran"

# (f) The state dir itself is writable by others: everything in it is ignored,
# even a sentinel and a good-looking environment file.
res="$(hook_case 'chmod 777 "$d/slurm_shim"; touch "$d/slurm_shim/environment.failed"; printf "export SLURM_JOB_NODELIST=evil\n" > "$d/slurm_shim/environment"; chmod 600 "$d/slurm_shim/environment"' '')"
assert_contains "$res" "BODY unset" "(f) world-writable state dir: sentinel and environment both ignored"
assert_contains "$res" "not a private directory" "(f) world-writable state dir: reported on stderr"

# (g) TMPDIR itself is writable by others (the OCS pre-creation shape): ignored.
res="$(hook_case 'chmod 777 "$d"; printf "export SLURM_JOB_NODELIST=evil\n" > "$d/slurm_shim/environment"; chmod 600 "$d/slurm_shim/environment"' '')"
assert_contains "$res" "BODY unset" "(g) world-writable TMPDIR: per-job state ignored"

# (h) The state dir is owned by someone else (root stands in for the co-tenant),
# holding a sentinel: ignored regardless of mode, job runs. Node-local /tmp on the
# master -- the shared home (virtiofs) does not honour a root chown reliably.
fd=/tmp/e2e-05-foreign.$$
manager "rm -rf '$fd'; mkdir -p '$fd/slurm_shim'; chown gridware:gridware '$fd'; chown root:root '$fd/slurm_shim'; chmod 755 '$fd' '$fd/slurm_shim'; : > '$fd/slurm_shim/environment.failed'"
res="$(gridware "printf '%s\n' 'export TMPDIR=$fd' '. $HOOK' 'echo \"BODY \${SLURM_JOB_NODELIST:-unset}\"' > /tmp/e2e-05-h.sh; bash /tmp/e2e-05-h.sh 2>&1; echo \"EXIT \$?\"; rm -f /tmp/e2e-05-h.sh")"
manager "rm -rf '$fd'"
assert_contains "$res" "not a private directory" "(h) foreign-owned state dir: reported and ignored"
assert_contains "$res" "BODY unset" "(h) foreign-owned state dir with sentinel: ignored, job body ran"
assert_contains "$res" "EXIT 0" "(h) foreign-owned state dir: exit 0"

# (i) No TMPDIR at all: no /tmp fallback, job continues (default policy).
res="$(gridware "printf '%s\n' 'unset TMPDIR' '. $HOOK' 'echo \"BODY \${SLURM_JOB_NODELIST:-unset}\"' > /tmp/e2e-05-notmp.sh; bash /tmp/e2e-05-notmp.sh; echo \"EXIT \$?\"; rm -f /tmp/e2e-05-notmp.sh")"
assert_contains "$res" "BODY unset" "(i) TMPDIR unset: no shared fallback, job continues"

finish
