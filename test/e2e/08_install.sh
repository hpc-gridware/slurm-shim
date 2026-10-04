#!/usr/bin/env bash
# Check: `slurm-shim install` plans without changing anything, applies
# idempotently, refuses to clobber a site's PE or starter, and `slurm-shim
# doctor` passes on the result. This is the installer the README points at, run
# against the real cluster.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "08_install: slurm-shim install / doctor"

S="$SHIM_PREFIX/bin/slurm-shim"
ADM="source /opt/ocs/default/common/settings.sh && $S"

# (1) Plan is read-only: the cluster is identical before and after.
before="$(manager "qconf -sp slurm-shim 2>/dev/null; qconf -sq all.q | grep -E '^(pe_list|starter_method)'")"
plan="$(manager "$ADM install --prefix $SHIM_PREFIX 2>&1")"
assert_contains "$plan" "Nothing was changed" "plan mode says it changed nothing"
after="$(manager "qconf -sp slurm-shim 2>/dev/null; qconf -sq all.q | grep -E '^(pe_list|starter_method)'")"
assert_eq "$after" "$before" "plan mode left the cluster identical"

# (2) Apply is idempotent: the second run applies zero changes.
manager "$ADM install --prefix $SHIM_PREFIX --apply >/dev/null 2>&1"
second="$(manager "$ADM install --prefix $SHIM_PREFIX --apply 2>&1")"
assert_contains "$second" "0 change(s) applied" "second --apply changes nothing"
assert_contains "$second" "ok        slurm-shim   start_proc_args" "the dedicated PE is reported unchanged"

# (3) The dedicated PE has the reference shape, and the queue offers it.
pe="$(manager "qconf -sp slurm-shim")"
assert_contains "$pe" "control_slaves       TRUE" "PE control_slaves TRUE"
assert_contains "$pe" "start_proc_args      $SHIM_PREFIX/bin/slurm-shim-env" "PE start_proc_args -> slurm-shim-env"
assert_contains "$pe" "allocation_rule      \$round_robin" "PE allocation_rule \$round_robin"
assert_contains "$(manager "qconf -sq all.q | grep ^pe_list")" "slurm-shim" "all.q offers the slurm-shim PE"

# (4) A foreign starter or PE is refused, never overwritten. A different
# --prefix makes both the existing PE's start_proc_args and all.q's starter
# "foreign" from the plan's point of view.
refusal="$(manager "$ADM install --prefix /opt/elsewhere --queue all 2>&1")"
assert_contains "$refusal" "REFUSED   all.q        starter_method" "existing starter_method is refused"
assert_contains "$refusal" "REFUSED   slurm-shim   start_proc_args" "existing PE start_proc_args is refused"
assert_contains "$refusal" "MPI" "the refusal explains the MPI hazard"
assert_eq "$(manager "qconf -sq all.q | grep ^starter_method | awk '{print \$2}'")" "$SHIM_PREFIX/bin/slurm-shim-starter" \
  "the refused starter was not changed"

# (5) The site config is merged, not rewritten: the test partitions survive
# alongside the installer's `all`.
cfg="$(manager "cat /opt/ocs/default/common/slurm-shim/config.yaml")"
assert_contains "$cfg" "batch:" "site partition batch survives the merge"
assert_contains "$cfg" "queue: all.q" "installer partition present"
assert_contains "$cfg" "memory_complex: mem_free" "safe memory default in the generated config"

# (6) Doctor passes on the configured cluster, as a normal user.
doc="$(gridware "source /opt/ocs/default/common/settings.sh && $S doctor 2>&1; echo EXIT=\$?")"
assert_contains "$doc" "EXIT=0" "doctor exits 0 on a healthy install"
assert_contains "$doc" "PASS  OCS 9." "doctor reports the OCS build"
assert_contains "$doc" "PASS  tree owned by root" "doctor verifies the install tree"
assert_contains "$doc" "PASS  queue all.q starter_method -> this tree" "doctor verifies the queue wiring"
case "$doc" in *"FAIL "*) fail "doctor reported a FAIL line: $(printf '%s\n' "$doc" | grep 'FAIL ' | head -3)" ;; *) pass "doctor has no FAIL lines" ;; esac

# (7) A first install on its own queue, and uninstall. A second tree and its
# own PE and config, so the harness install above is not touched: by default
# install creates slurm.q (a clone of all.q, entered only through the FORCED
# complex slurm_shim) and changes no existing queue; uninstall --apply
# --purge-config puts the cluster back exactly as it was.
T=/opt/shimtest
snap() { manager "qconf -sql | sort; qconf -spl | sort; qconf -sc | grep -c '^slurm_shim ' || true; qconf -sq all.q | grep -E '^(pe_list|starter_method)'"; }
cluster_before="$(snap)"
first="$(manager "$ADM install --apply --prefix $T --pe shimtest --config /tmp/shimtest.yaml 2>&1")"
assert_contains "$first" "add       queue slurm.q cloned from all.q, complex_values slurm_shim=TRUE" \
  "a first install creates slurm.q from all.q, behind the FORCED complex"
assert_eq "$(manager "qconf -sq all.q | grep -E '^(pe_list|starter_method)'")" \
  "$(printf '%s\n' "$cluster_before" | grep -E '^(pe_list|starter_method)')" "all.q is untouched"
assert_contains "$(manager "qconf -sq slurm.q | grep -E '^(pe_list|starter_method)'")" "$T/bin/slurm-shim-starter" \
  "slurm.q runs the new tree's starter"
assert_contains "$(manager "cat /tmp/shimtest.yaml")" "request: slurm_shim=TRUE" "the slurm partition requests the complex"
dry="$(gridware "source /opt/ocs/default/common/settings.sh && SLURM_SHIM_CONFIG=/tmp/shimtest.yaml SLURM_SHIM_DRY_RUN=1 $T/bin/sbatch -p slurm --wrap true 2>&1")"
assert_contains "$dry" "slurm_shim=TRUE" "sbatch on the slurm partition requests the complex"
# A site job that does not ask for the shim never enters slurm.q.
plain="$(gridware "qsub -w v -q slurm.q -b y true 2>&1; true")"
assert_contains "$plain" "no suitable queues" "a job without -l slurm_shim cannot enter slurm.q"
# A native job on the new queue, pinned to the master (the only host with $T).
ran="$(gridware "qsub -sync y -q slurm.q -l slurm_shim=TRUE,h=$MASTER -b y true >/dev/null 2>&1; echo rc=\$?")"
assert_contains "$ran" "rc=0" "a job that requests the complex runs on slurm.q"
again="$(manager "$ADM install --apply --prefix $T --pe shimtest --config /tmp/shimtest.yaml 2>&1")"
assert_contains "$again" "0 change(s) applied" "a re-run keeps slurm.q and changes nothing"
uplan="$(manager "$S uninstall --prefix $T 2>&1")"
assert_contains "$uplan" "delete    queue slurm.q" "uninstall plans to delete the queue it created"
assert_contains "$uplan" "delete    complex slurm_shim" "and the complex"
assert_contains "$uplan" "Nothing was changed" "the uninstall plan changes nothing"
# A pending job that needs slurm.q stops uninstall before it changes anything.
held="$(gridware "qsub -terse -h -q slurm.q -l slurm_shim=TRUE -pe shimtest 1 -b y true" | tr -d '\r')"
blocked="$(manager "$ADM uninstall --apply --purge-config --config /tmp/shimtest.yaml --prefix $T 2>&1; echo rc=\$?")"
assert_contains "$blocked" "job(s) $held" "a pending job blocks uninstall, named"
assert_contains "$(manager "qconf -sq slurm.q | grep ^starter_method")" "$T/bin/slurm-shim-starter" \
  "the blocked uninstall changed nothing"
gridware "qdel $held >/dev/null 2>&1"
undo="$(manager "$ADM uninstall --apply --purge-config --config /tmp/shimtest.yaml --prefix $T 2>&1; echo rc=\$?")"
assert_contains "$undo" "rc=0" "uninstall --apply succeeds"
assert_eq "$(snap)" "$cluster_before" "uninstall puts queues, PEs, the complex and all.q back exactly"
assert_eq "$(manager "ls $T /tmp/shimtest.yaml 2>/dev/null | wc -l" | tr -d ' ')" "0" \
  "uninstall removes the tree and, with --purge-config, the config"
# A wrong prefix removes nothing: no shim tree there, nothing points at it.
wrong="$(manager "mkdir -p /tmp/notashim/bin && echo real > /tmp/notashim/bin/sbatch && $ADM uninstall --apply --prefix /tmp/notashim 2>&1; cat /tmp/notashim/bin/sbatch; rm -rf /tmp/notashim")"
assert_contains "$wrong" "not a slurm-shim install tree" "a directory without bin/slurm-shim is not a shim tree"
assert_contains "$wrong" "real" "and its files are untouched"

# (8) --version names the scheduler build; the shimmed -V stays SLURM-parsable.
ver="$(gridware "source /opt/ocs/default/common/settings.sh && $S --version")"
assert_contains "$ver" "Open Cluster Scheduler 9." "base binary --version names the OCS build"
srunv="$(gridware "srun --version")"
case "$srunv" in "slurm "*) pass "srun --version stays SLURM-parsable: $srunv" ;; *) fail "srun --version changed shape: $srunv" ;; esac

finish
