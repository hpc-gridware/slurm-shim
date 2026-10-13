#!/usr/bin/env bash
# Check: the cluster resources a SLURM job names reach Grid Engine and take
# effect there -- licenses (-L), node features (-C), a whole node
# (--exclusive), hosts to avoid or use (-x, -w) and --mem-per-cpu -- from
# sbatch, salloc and srun outside an allocation.
#
# The installer never creates license, feature or exclusive complexes; a site
# does. This check adds three for its run and removes them on exit:
#   e2e_lic  INT  consumable JOB, 3 on the global host (a license count)
#   e2e_avx  BOOL true on one host only (a node feature)
#   e2e_excl BOOL relop EXCL on every host (exclusive placement)
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "23_resources: licenses, features, --exclusive, -x/-w, --mem-per-cpu"

HOSTS="$(manager "qconf -sel" | tr '\n' ' ')"
FEATURE_HOST="$(printf '%s\n' $HOSTS | sed -n 2p)"
[ -n "$FEATURE_HOST" ] || FEATURE_HOST="$(printf '%s\n' $HOSTS | sed -n 1p)"
OTHER_HOST="$(printf '%s\n' $HOSTS | grep -vx "$FEATURE_HOST" | head -1)"

CLEANUP_IDS=""
CLEANUP_DONE=""
# remove_complexes deletes this check's jobs, waits until none is left (Grid
# Engine keeps a complex that a job still requests), then removes the
# complexes. It prints what is left, nothing when the cluster is clean.
remove_complexes() {
  [ -n "$CLEANUP_IDS" ] && gridware "qdel $CLEANUP_IDS >/dev/null 2>&1" || true
  for _ in $(seq 1 60); do
    left=""
    for id in $CLEANUP_IDS; do [ -n "$(jstate "$id")" ] && left="$left $id"; done
    [ -z "$left" ] && break
    sleep 1
  done
  manager "
    qconf -dattr exechost complex_values e2e_lic=3 global >/dev/null 2>&1
    qconf -dattr exechost complex_values e2e_avx=true $FEATURE_HOST >/dev/null 2>&1
    for h in $HOSTS; do qconf -dattr exechost complex_values e2e_excl=true \$h >/dev/null 2>&1; done
    qconf -sc | grep -v '^e2e_' > /tmp/e2e23.sc && qconf -Mc /tmp/e2e23.sc >/dev/null 2>&1
    rm -f /tmp/e2e23.sc" || true
  manager "qconf -sc | grep '^e2e_'" || true
}
# On an abort the complexes still go, and a failure to remove them is loud.
cleanup() {
  [ -n "$CLEANUP_DONE" ] && return
  left="$(remove_complexes)"
  [ -n "$left" ] && printf '23_resources: complexes left on the cluster:\n%s\n' "$left" >&2 && exit 1
  return 0
}
trap cleanup EXIT

manager "
  qconf -sc | grep -v '^e2e_' > /tmp/e2e23.sc
  cat >> /tmp/e2e23.sc <<EOF
e2e_lic   e2elic  INT  <=   YES YES 0 0
e2e_avx   e2eavx  BOOL ==   YES NO  0 0
e2e_excl  e2eexcl BOOL EXCL YES YES 0 1000
EOF
  sed -i 's/^e2e_lic .*/e2e_lic   e2elic  INT  <=   YES JOB 0 0/' /tmp/e2e23.sc
  qconf -Mc /tmp/e2e23.sc >/dev/null
  qconf -mattr exechost complex_values e2e_lic=3 global >/dev/null
  qconf -aattr exechost complex_values e2e_avx=true $FEATURE_HOST >/dev/null
  for h in $HOSTS; do qconf -aattr exechost complex_values e2e_excl=true \$h >/dev/null; done
  rm -f /tmp/e2e23.sc"

submit() { gridware "cd && sbatch $* </dev/null" 2>/dev/null | awk '/Submitted batch job/{print $NF}' || true; }
jattr() {
  gridware "qstat -j '$1' 2>/dev/null | awk -F: '/^$2/{sub(/^[^:]*:/,\"\"); print; exit}'" | tr -d '[:space:]'
}
jstate() { gridware "qstat 2>/dev/null | awk '\$1==$1 {print \$5; exit}'"; }
jhost() { gridware "qstat 2>/dev/null | awk '\$1==$1 {print \$8; exit}'" | sed 's/.*@//'; }
# wait_running <id> waits up to 60s for the job to run.
wait_running() {
  for _ in $(seq 1 60); do case "$(jstate "$1")" in r) return 0 ;; "") return 1 ;; esac; sleep 1; done
  return 1
}

# -- licenses ---------------------------------------------------------------
a="$(submit "-L e2e_lic:2 -J e2e23a --wrap='sleep 60'")"
CLEANUP_IDS="$CLEANUP_IDS $a"
assert_contains "$(jattr "$a" hard_resource_list)" "e2e_lic=2" "sbatch -L reaches Grid Engine as a license request"
wait_running "$a" && pass "the first 2-license job runs" || fail "the first 2-license job did not start"
b="$(submit "-L e2e_lic:2 -J e2e23b --wrap='sleep 5'")"
CLEANUP_IDS="$CLEANUP_IDS $b"
sleep 8
assert_eq "$(jstate "$b")" "qw" "a second 2-license job waits: 3 licenses cannot cover 4"
gridware "qdel $a >/dev/null 2>&1" || true
wait_running "$b" && pass "and it starts once the first job frees its licenses" \
  || fail "the second license job did not start after the first was deleted"
gridware "qdel $b >/dev/null 2>&1" || true

res="$(gridware "cd && sbatch -L calibre:1 --wrap=true </dev/null 2>&1; echo rc=\$?")"
assert_contains "$res" 'unknown license "calibre"' "an unknown license is refused"
assert_contains "$res" "rc=1" "and nothing is submitted"

# A JOB-scope license spans hosts once: a 2-node job takes 2 of the 3.
m="$(submit "-N 2 --ntasks-per-node=1 -L e2e_lic:2 -J e2e23m --wrap='sleep 30'")"
CLEANUP_IDS="$CLEANUP_IDS $m"
if wait_running "$m"; then
  avail="$(gridware "qstat -F e2e_lic -q all.q 2>/dev/null | awk -F= '/e2e_lic/{print \$2; exit}'")"
  assert_eq "$avail" "1" "a 2-node job holds 2 licenses, not 2 per host"
else
  fail "the 2-node license job did not start"
fi
gridware "qdel $m >/dev/null 2>&1" || true

# -- features ---------------------------------------------------------------
f="$(submit "-C e2e_avx -J e2e23f --wrap='sleep 20'")"
CLEANUP_IDS="$CLEANUP_IDS $f"
wait_running "$f" && assert_eq "$(jhost "$f")" "$FEATURE_HOST" "sbatch -C runs on the host with the feature" \
  || fail "the -C job did not start"
res="$(gridware "cd && sbatch -C 'e2e_avx|nope' --wrap=true </dev/null 2>&1; echo rc=\$?")"
assert_contains "$res" "only features joined by &" "an either-or constraint is refused"

# -- hosts ------------------------------------------------------------------
x="$(submit "-x $FEATURE_HOST -J e2e23x --wrap='sleep 20'")"
CLEANUP_IDS="$CLEANUP_IDS $x"
if wait_running "$x"; then
  h="$(jhost "$x")"
  [ "$h" != "$FEATURE_HOST" ] && pass "sbatch -x keeps the job off $FEATURE_HOST (ran on $h)" \
    || fail "sbatch -x ran on the excluded host"
else
  fail "the -x job did not start"
fi
w="$(submit "-w $OTHER_HOST -J e2e23w --wrap='sleep 20'")"
CLEANUP_IDS="$CLEANUP_IDS $w"
wait_running "$w" && assert_eq "$(jhost "$w")" "$OTHER_HOST" "sbatch -w runs on the named host" \
  || fail "the -w job did not start"

# -- exclusive --------------------------------------------------------------
e="$(submit "--exclusive -w $OTHER_HOST -J e2e23e --wrap='sleep 30'")"
[ -n "$e" ] || gridware "cd && sbatch --exclusive -w $OTHER_HOST --wrap=true </dev/null" >&2
CLEANUP_IDS="$CLEANUP_IDS $e"
assert_contains "$(jattr "$e" hard_resource_list)" "e2e_excl=true" "sbatch --exclusive requests the exclusive complex"
gridware "qdel $w >/dev/null 2>&1" || true
if wait_running "$e"; then
  o="$(submit "-w $OTHER_HOST -J e2e23o --wrap='sleep 5'")"
  CLEANUP_IDS="$CLEANUP_IDS $o"
  sleep 8
  assert_eq "$(jstate "$o")" "qw" "another job waits while --exclusive holds the host"
  gridware "qdel $e >/dev/null 2>&1" || true
  wait_running "$o" && pass "and it starts once the exclusive job is gone" \
    || fail "the job did not start after the exclusive job was deleted"
else
  fail "the --exclusive job did not start"
fi
gridware "qdel $e $f $x $o >/dev/null 2>&1" || true

# -- memory per CPU ---------------------------------------------------------
c="$(submit "-H -N 1 -n 2 -c 2 --mem-per-cpu=100M -J e2e23c --wrap=true")"
u="$(submit "-H -n 2 -c 2 --mem-per-cpu=100M -J e2e23u --wrap=true")"
CLEANUP_IDS="$CLEANUP_IDS $c $u"
mc="$(manager "grep -E '^memory_complex' \$(slurm-shim config path 2>/dev/null) 2>/dev/null | awk '{print \$2}'")"
[ -n "$mc" ] || mc=mem_free
assert_contains "$(jattr "$c" hard_resource_list)" "$mc=400M" "--mem-per-cpu=100M x 4 CPUs on one node is 400M per node"
# Without -N nothing pins the tasks per host: one task's memory, not all of it.
assert_contains "$(jattr "$u" hard_resource_list)" "$mc=200M" "--mem-per-cpu without -N asks one task's 200M per host"
gridware "qdel $c $u >/dev/null 2>&1" || true

# -- salloc and srun outside an allocation ----------------------------------
res="$(gridware "cd && timeout -s INT 120 salloc -C e2e_avx -L e2e_lic:1 hostname </dev/null 2>&1; echo rc=\$?")"
assert_contains "$res" "rc=0" "salloc with -C and -L runs"
assert_contains "$res" "$FEATURE_HOST" "salloc -C runs on the host with the feature"
res="$(gridware "cd && timeout -s INT 120 srun -x $FEATURE_HOST hostname </dev/null 2>&1; echo rc=\$?")"
assert_contains "$res" "rc=0" "srun -x outside an allocation runs"
case "$res" in *"$FEATURE_HOST"*) fail "srun -x ran on the excluded host" ;; *) pass "srun -x keeps the step off $FEATURE_HOST" ;; esac
res="$(gridware "cd && timeout -s INT 120 srun -w $OTHER_HOST hostname </dev/null 2>&1; echo rc=\$?")"
assert_contains "$res" "$OTHER_HOST" "srun -w outside an allocation runs on the named host"

assert_eq "$(remove_complexes)" "" "the check's complexes are removed from the cluster"
CLEANUP_DONE=1
finish
