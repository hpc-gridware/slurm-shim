#!/usr/bin/env bash
# Check: srun pins tasks only within the CPUs the job may use (CS-2945).
#
#   shared   two 2-task jobs on one node, no Grid Engine binding: the tasks are
#            not pinned (any CPU they picked could be the other job's)
#   bound    two jobs with core binding (-bamount) on one node: every task, and
#            every OpenMPI rank under srun --mpi=pmix, stays in its job's cores,
#            each task on its own CPU
#   nodes    a 2-node bound job whose cores do not start at CPU 0 (a holder job
#            takes that core first): on each node the pinned task's CPU lies in
#            the job's cores there (reached through qrsh -inherit)
#   owned    a job holding every slot the host offers: one CPU per task
#
# Every case with OpenMPI also runs a plain mpirun from the job script, which
# the job environment keeps within the job's CPUs the same way.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "25_cpu_binding: tasks pinned only within the job's CPUs"

d=$JOB_HOME/e2e-25
gridware "rm -rf $d && mkdir -p $d"
holder=""
cleanup() {
  rm -f "${tmpjob:-}"
  [ -n "$holder" ] && gridware "scancel '$holder'" >/dev/null 2>&1
  true
}
trap cleanup EXIT

# One exec host for the jobs that must share a node; not the master if possible.
host="$(manager "qconf -sel 2>/dev/null | cut -d. -f1" | grep -vx "$MASTER" | head -1 | tr -d '[:space:]')"
[ -n "$host" ] || host="$(queue_host)"
queue="${E2E_QUEUE:-all.q}"
bindable=0
gridware "qsub -help 2>&1 | grep -q -- -bamount" && bindable=1
ompi=""
for h in ${OPENMPI_HOMES:-/opt/ocs/openmpi5 /usr/lib64/mpi/gcc/openmpi4}; do
  gridware "[ -x $h/bin/mpirun ]" && ompi="$ompi $h"
done

# cpus <list> expands a CPU list ("0-2,5") to one CPU per line.
cpus() {
  local IFS=, p
  for p in $1; do
    case "$p" in
      *-*) seq "${p%-*}" "${p#*-}" ;;
      *) echo "$p" ;;
    esac
  done
}
# within <list> <mask> succeeds when every CPU of list is in mask.
within() { [ -z "$(comm -23 <(cpus "$1" | sort) <(cpus "$2" | sort))" ]; }
# single <list> succeeds when list names exactly one CPU.
single() { [ "$(cpus "$1" | wc -l | tr -d ' ')" = 1 ]; }
# field <line> <key> prints key's value from a "k=v k=v" line.
field() { printf '%s\n' "$1" | tr ' ' '\n' | sed -n "s/^$2=//p"; }
# count <text> counts its non-empty lines.
count() { printf '%s\n' "$1" | grep -c . || true; }

# TASK lines: step=<name> rank=<n> host=<h> mask=<cpus>.
report='echo "TASK step=$STEP rank=$SLURM_PROCID host=$(hostname) mask=$(grep Cpus_allowed_list /proc/self/status | cut -f2)"'

# put <name> <script text> writes a job script to the master; echoes its path.
put() {
  tmpjob="$(mktemp)"
  printf '%s\n' "$2" >"$tmpjob"
  put_job "$tmpjob" "$d/$1.sh"
  rm -f "$tmpjob"
  echo "$d/$1.sh"
}
# submit <name> <script text> submits a job; echoes its id, empty when sbatch
# refused it. Never fails: the harness runs under set -e (cluster/lib.sh), and
# a refused job must be reported, not end the check without a summary.
submit() { sbatch_submit "$(put "$1" "$2")" "$d/$1.out" || true; }

# pairjob <pair> <header> is a job for a pair: the batch mask, then a barrier
# until both jobs of the pair run (TOGETHER yes/no), then its steps.
pairjob() {
  printf '#!/bin/bash\n%s\n' "$2"
  echo 'echo "BATCH mask=$(grep Cpus_allowed_list /proc/self/status | cut -f2)"'
  echo "touch $d/\$JOB_ID.$1.started"
  echo "for _ in \$(seq 1 60); do [ \$(ls $d/*.$1.started | wc -l) -ge 2 ] && break; sleep 1; done"
  echo "[ \$(ls $d/*.$1.started | wc -l) -ge 2 ] && echo TOGETHER yes || echo TOGETHER no"
  echo "STEP=plain srun -n 2 bash -c '$report'"
  for h in $ompi; do
    echo "PATH=$h/bin:\$PATH LD_LIBRARY_PATH=$h/lib:$h/lib64 PMIX_MCA_pcompress_base_silence_warning=1 STEP=$(basename "$h") srun --mpi=pmix -n 2 bash -c '$report'"
    echo "PATH=$h/bin:\$PATH LD_LIBRARY_PATH=$h/lib:$h/lib64 PMIX_MCA_pcompress_base_silence_warning=1 STEP=mpirun-$(basename "$h") mpirun -n 2 bash -c '$report'"
  done
}
# pair <name> <header> runs two copies at once; outputs in pair_a and pair_b.
pair() {
  local a b
  a="$(submit "$1-a" "$(pairjob "$1" "$2")")"
  b="$(submit "$1-b" "$(pairjob "$1" "$2")")"
  pair_a="$(jobout "$a" "$d/$1-a.out")"
  pair_b="$(jobout "$b" "$d/$1-b.out")"
}

# --- shared: no binding, 2 of the node's slots ------------------------------
pair shared "#SBATCH -w $host -n 2"
for out in "$pair_a" "$pair_b"; do
  batch="$(printf '%s\n' "$out" | sed -n 's/^BATCH mask=//p')"
  tasks="$(printf '%s\n' "$out" | grep '^TASK step=plain' || true)"
  assert_eq "$(count "$tasks")" "2" "shared: the job ran 2 tasks"
  assert_eq "$(printf '%s\n' "$tasks" | grep -vc " mask=$batch\$" || true)" "0" \
    "shared: no task is pinned on a node shared without binding (each keeps ${batch:-?})"
  for h in $ompi; do
    ranks="$(printf '%s\n' "$out" | grep "^TASK step=mpirun-$(basename "$h") " || true)"
    assert_eq "$(count "$ranks")/$(printf '%s\n' "$ranks" | grep -vc " mask=$batch\$" || true)" "2/0" \
      "shared: plain mpirun ($(basename "$h")) leaves its ranks unpinned"
  done
done

# An unbound job's mask is every CPU of the host.
wholehost="${batch:-}"

# --- bound: Grid Engine core binding, two jobs on one node ------------------
# check_bound <label> <output> checks one job of the bound pair and sets
# bound_mask to its cores. Not run in $(...): the assertions must count.
check_bound() {
  local label="$1" out="$2" mask plain bad line h ranks
  mask="$(printf '%s\n' "$out" | sed -n 's/^BATCH mask=//p')"
  bound_mask=""
  # Too few cores for the pair: Grid Engine grants the whole host, which is
  # no restriction to stay in, and with half the slots the job shares the
  # host -- unpinned is right, and there is nothing to check here.
  if [ -n "$mask" ] && [ "$mask" = "$wholehost" ]; then
    skip "bound ($label): binding granted the whole host ($mask), too few cores for the pair"
    return
  fi
  plain="$(printf '%s\n' "$out" | grep '^TASK step=plain' || true)"
  assert_eq "$(count "$plain")" "2" "bound ($label): 2 tasks ran"
  bad=0
  while read -r line; do
    { single "$(field "$line" mask)" && within "$(field "$line" mask)" "$mask"; } || bad=$((bad + 1))
  done <<<"$plain"
  assert_eq "$bad" "0" "bound ($label): each task pinned to one CPU of its job's cores (${mask:-?})"
  assert_eq "$(printf '%s\n' "$plain" | sed 's/.* mask=//' | sort -u | grep -c . || true)" "2" \
    "bound ($label): the 2 tasks are on different CPUs"
  for h in $ompi; do
    ranks="$(printf '%s\n' "$out" | grep "^TASK step=$(basename "$h") " || true)"
    bad=0
    while read -r line; do
      within "$(field "$line" mask)" "$mask" || bad=$((bad + 1))
    done <<<"$ranks"
    assert_eq "$(count "$ranks")/$bad" "2/0" \
      "bound ($label): $(basename "$h") ranks under srun --mpi=pmix stay in the job's cores (${mask:-?})"
    ranks="$(printf '%s\n' "$out" | grep "^TASK step=mpirun-$(basename "$h") " || true)"
    bad=0
    while read -r line; do
      within "$(field "$line" mask)" "$mask" || bad=$((bad + 1))
    done <<<"$ranks"
    assert_eq "$(count "$ranks")/$bad" "2/0" \
      "bound ($label): plain mpirun ($(basename "$h")) ranks stay in the job's cores (${mask:-?})"
  done
  bound_mask="$mask"
}

if [ "$bindable" = 0 ]; then
  skip "bound/nodes: this OCS has no -bamount core binding"
else
  pair bound "#SBATCH -w $host -n 2
#\$ -bamount 1 -bunit C"
  check_bound "first job" "$pair_a"
  mask_a="$bound_mask"
  check_bound "second job" "$pair_b"
  mask_b="$bound_mask"
  # Grid Engine overlaps the grants when the host has fewer cores than the
  # pair asks for (2 jobs x 2 cores); the CI runners have 2.
  cores="$(node_sh "$host" "lscpu -p=core,socket | grep -v '^#' | sort -u | wc -l" | tr -d '[:space:]')"
  if ! printf '%s\n%s\n' "$pair_a" "$pair_b" | grep -q "^TOGETHER yes"; then
    skip "bound: the two jobs never ran at the same time, so their cores may legitimately match"
  elif [ "${cores:-0}" -lt 4 ]; then
    skip "bound: $host has ${cores:-?} cores, too few for two jobs of 2 cores without overlap"
  elif [ -n "$mask_a" ] && [ -n "$mask_b" ]; then
    assert_eq "$(comm -12 <(cpus "$mask_a" | sort) <(cpus "$mask_b" | sort) | grep -c . || true)" "0" \
      "bound: the two jobs' cores ($mask_a, $mask_b) do not overlap"
  fi

  # --- nodes: a bound job on 2 nodes, its cores not starting at CPU 0 -------
  if [ "${EXEC_NODES:-0}" -lt 2 ]; then
    skip "nodes: needs 2 exec nodes"
  else
    holder="$(submit holder "#!/bin/bash
#SBATCH -N $EXEC_NODES --ntasks-per-node=1 -o /dev/null
#\$ -bamount 1 -bunit C
sleep 120")"
    state=""
    for _ in $(seq 1 30); do
      state="$(gridware "squeue -h -j '$holder' -o %t" 2>/dev/null | tr -d '[:space:]')"
      [ "$state" = R ] && break
      sleep 2
    done
    if [ "$state" != R ]; then
      fail "nodes: the holder job never started (state '${state:-gone}'), so the check would prove nothing"
    else
      id="$(submit nodes "#!/bin/bash
#SBATCH -N 2 --ntasks-per-node=2
#\$ -bamount 1 -bunit C
STEP=grant srun -N 2 --ntasks-per-node=1 --cpu-bind=none bash -c '$report'
STEP=pinned srun -N 2 --ntasks-per-node=1 bash -c '$report'
$(for h in $ompi; do echo "PATH=$h/bin:\$PATH LD_LIBRARY_PATH=$h/lib:$h/lib64 PMIX_MCA_pcompress_base_silence_warning=1 STEP=mpirun-$(basename "$h") mpirun -n 4 bash -c '$report'"; done)")"
      if [ -z "$id" ]; then
        # With the holder's core taken, a host needs 3 cores for this job; Grid
        # Engine checks free cores at submit (-w e) and refuses it on 2.
        skip "nodes: Grid Engine refused the bound 2-node job beside the holder (too few cores per host)"
      else
      res="$(jobout "$id" "$d/nodes.out")"
      pinned="$(printf '%s\n' "$res" | grep '^TASK step=pinned' || true)"
      assert_eq "$(printf '%s\n' "$pinned" | awk '{print $4}' | sort -u | grep -c . || true)" "2" \
        "nodes: the pinned step ran on 2 nodes"
      bad=0
      at0=0
      while read -r line; do
        h="$(field "$line" host)"
        grant="$(field "$(printf '%s\n' "$res" | grep "^TASK step=grant .* host=$h " || true)" mask)"
        within 0 "$grant" && at0=$((at0 + 1))
        { [ -n "$grant" ] && within "$(field "$line" mask)" "$grant"; } || bad=$((bad + 1))
      done <<<"$pinned"
      if [ "$at0" -gt 0 ]; then
        skip "nodes: a grant still includes CPU 0 (the holder did not take it); pinning from CPU 0 would pass unnoticed"
      else
        assert_eq "$bad" "0" "nodes: on each node the task's CPU is in the job's cores there, which exclude CPU 0"
      fi
      # A plain mpirun -n 4 (2 ranks per node: OpenMPI would bind to a whole
      # socket) keeps every rank in the job's cores on its node.
      for h in $ompi; do
        ranks="$(printf '%s\n' "$res" | grep "^TASK step=mpirun-$(basename "$h") " || true)"
        bad=0
        while read -r line; do
          h2="$(field "$line" host)"
          grant="$(field "$(printf '%s\n' "$res" | grep "^TASK step=grant .* host=$h2 " || true)" mask)"
          { [ -n "$grant" ] && within "$(field "$line" mask)" "$grant"; } || bad=$((bad + 1))
        done <<<"$ranks"
        assert_eq "$(count "$ranks")/$bad" "4/0" \
          "nodes: plain mpirun ($(basename "$h")) -n 4 keeps every rank in the job's cores on its node"
      done
      fi
    fi
    gridware "scancel '$holder'" >/dev/null 2>&1
    holder=""
  fi
fi

# --- owned: every slot the host offers --------------------------------------
# Grid Engine decides ownership: all slots of all queues on the host, not the
# CPU count (a site may give a slot per core or per thread).
qslots="$(manager "qstat -f -q $queue 2>/dev/null | awk '\$1 ~ /@$host([.]|\$)/ {split(\$3, s, \"/\"); print s[3]}'" | head -1 | tr -d '[:space:]')"
hslots="$(manager "qstat -f 2>/dev/null | awk '\$1 ~ /@$host([.]|\$)/ {split(\$3, s, \"/\"); t += s[3]} END {print t}'" | tr -d '[:space:]')"
if [ -z "$qslots" ] || [ "${qslots:-0}" -lt 2 ] || [ "$qslots" != "$hslots" ]; then
  skip "owned: $queue@$host offers ${qslots:-?} of the host's ${hslots:-?} slots (set E2E_QUEUE for another queue)"
else
  id="$(submit owned "#!/bin/bash
#SBATCH -w $host -n $qslots
STEP=owned srun -n 2 bash -c '$report'")"
  res="$(jobout "$id" "$d/owned.out")"
  masks="$(printf '%s\n' "$res" | grep '^TASK step=owned' | sed 's/.* mask=//' | sort -u)"
  assert_eq "$(printf '%s\n' "$masks" | grep -c '^[0-9]*$' || true)" "2" \
    "owned: a job holding all $qslots slots of the host pins its 2 tasks to 2 different CPUs"
fi
finish
