#!/usr/bin/env bash
# Check: OpenMPI runs through the shim, both as a plain mpirun in a batch job
# and as srun --mpi=pmix, for every OpenMPI the cluster has.
#
# mpirun sees SLURM_JOB_ID and starts its daemons with the shim's srun (an
# external-launcher step: unpinned, on the nodes it names, or on the nodes
# other than the master when PRRTE omits the list). srun --mpi=pmix execs
# mpirun for the step, with each rank's SLURM_* named by rank-env.
#
# Needs OpenMPI on every node: make cluster-openmpi [OPENMPI5=1]. OPENMPI_HOMES
# (space-separated install prefixes) overrides the ones probed.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "35_mpi_pmix: OpenMPI through plain mpirun and srun --mpi=pmix"

if [ "${EXEC_NODES:-0}" -lt 2 ]; then
  skip "needs 2 exec nodes (cluster has ${EXEC_NODES:-0})"
  finish
fi
homes=""
for h in ${OPENMPI_HOMES:-/opt/ocs/openmpi5 /usr/lib64/mpi/gcc/openmpi4}; do
  if gridware "[ -x $h/bin/mpicc ] && [ -x $h/bin/mpirun ]"; then
    homes="$homes $h"
  fi
done
if [ -z "$homes" ]; then
  skip "no OpenMPI on the cluster (make cluster-openmpi [OPENMPI5=1], or set OPENMPI_HOMES)"
  finish
fi

job=""
trap 'rm -f "$job"' EXIT
d=$JOB_HOME/e2e-35
gridware "rm -rf $d && mkdir -p $d"
put_job "$E2E_DIR/mpi/hello.c" "$d/hello.c"

nodes=$EXEC_NODES
n=$((nodes * 2))
sub=$((nodes - 1))

# section <name> prints the job output between "== <name>" and the next "== ".
section() { printf '%s\n' "$res" | awk -v s="== $1" '$0 == s {f = 1; next} /^== / {f = 0} f'; }
hellos() { section "$1" | grep -c '^HELLO' || true; }
hosts() { section "$1" | grep '^HELLO' | sed 's/.* host=\([^ ]*\).*/\1/' | sort -u | wc -l | tr -d ' '; }

for h in $homes; do
  tag="$(basename "$h")"
  if ! gridware "$h/bin/mpicc -O2 -o $d/hello-$tag $d/hello.c"; then
    fail "$tag: mpicc could not build the test program"
    continue
  fi
  job="$(mktemp)"
  cat >"$job" <<EOF
#!/bin/bash
#SBATCH --nodes=$nodes
#SBATCH --ntasks-per-node=2
export PATH=$h/bin:\$PATH LD_LIBRARY_PATH=$h/lib:$h/lib64
p=$d/hello-$tag
echo "== mpirun";      timeout 120 mpirun -n $n \$p; echo "rc=\$?"
echo "== bind-none";   timeout 120 mpirun --bind-to none -n $n \$p; echo "rc=\$?"
echo "== pmix";        timeout 120 srun --mpi=pmix -n $n \$p; echo "rc=\$?"
echo "== subset";      timeout 120 srun --mpi=pmix -N $sub --ntasks-per-node=2 \$p; echo "rc=\$?"
echo "== fail";        timeout 120 srun --mpi=pmix -n $n \$p 1; echo "rc=\$?"
echo "== vars";        timeout 120 srun --mpi=pmix -n $n bash -c 'i=0; for h in \$(scontrol show hostnames \$SLURM_STEP_NODELIST); do [ "\$h" = "\$(hostname)" ] && break; i=\$((i+1)); done; echo "VARS nodeid=\$SLURM_NODEID want=\$i ntasks=\$SLURM_NTASKS"'; echo "rc=\$?"
last=\$(scontrol show hostnames \$SLURM_JOB_NODELIST | grep -vx "\$(hostname)" | tail -1)
echo "== nomaster";    echo "want=\$last"; timeout 120 srun --mpi=pmix -w \$last -n 2 \$p; echo "rc=\$?"
echo "== end"
EOF
  remote=$d/job-$tag.sh
  out=$d/job-$tag.out
  put_job "$job" "$remote"
  id="$(sbatch_submit "$remote" "$out")"
  res="$(jobout "$id" "$out")"

  # Plain mpirun: every rank up, wired (sum = size), on every node.
  assert_eq "$(hellos mpirun)" "$n" "$tag: plain mpirun starts $n ranks"
  assert_eq "$(section mpirun | grep '^HELLO' | grep -vc " size=$n .* sum=$n " || true)" "0" \
    "$tag: plain mpirun wires every rank (size and all-reduce sum $n)"
  assert_eq "$(hosts mpirun)" "$nodes" "$tag: plain mpirun uses all $nodes nodes"
  assert_contains "$(section mpirun)" "rc=0" "$tag: plain mpirun exits 0"

  # A daemon started by srun must not pin the ranks it forks to one CPU.
  assert_eq "$(section bind-none | grep -c ' cpus=0 ' || true)" "0" \
    "$tag: with --bind-to none no rank is confined to CPU 0"

  # srun --mpi=pmix: ranks wired, and SLURM_PROCID is the MPI rank.
  assert_eq "$(hellos pmix)" "$n" "$tag: srun --mpi=pmix starts $n ranks"
  assert_eq "$(section pmix | grep '^HELLO' | awk '{split($2, r, "="); split($NF, p, "="); if (r[2] != p[2]) bad++} END {print bad + 0}')" "0" \
    "$tag: SLURM_PROCID in each rank is its MPI rank"
  assert_contains "$(section pmix)" "rc=0" "$tag: srun --mpi=pmix exits 0"

  # A step on part of the allocation stays on that part.
  assert_eq "$(hellos subset)" "$((sub * 2))" "$tag: srun --mpi=pmix -N $sub starts $((sub * 2)) ranks"
  assert_eq "$(hosts subset)" "$sub" "$tag: srun --mpi=pmix -N $sub uses $sub nodes"

  # A rank forked by a daemon names this step and its own node, not the
  # daemon step's.
  assert_eq "$(section vars | grep -c '^VARS' || true)" "$n" "$tag: srun --mpi=pmix runs $n shell ranks"
  assert_eq "$(section vars | grep '^VARS' | awk -v n="$n" '{split($2, a, "="); split($3, w, "="); split($4, t, "="); if (a[2] != w[2] || t[2] != n) bad++} END {print bad + 0}')" "0" \
    "$tag: SLURM_NODEID and SLURM_NTASKS in each rank name the pmix step"

  # A step that leaves out the master runs on the node it asked for.
  want="$(section nomaster | sed -n 's/^want=//p')"
  assert_eq "$(hellos nomaster)" "2" "$tag: srun --mpi=pmix -w <one node> starts 2 ranks"
  assert_eq "$(section nomaster | grep '^HELLO' | grep -vc " host=$want " || true)" "0" \
    "$tag: srun --mpi=pmix -w $want runs every rank on $want, a node the batch shell is not on"

  # A failing rank fails the step, not the job.
  assert_contains "$(section fail)" "rc=3" "$tag: a rank that exits 3 makes srun exit 3"
  assert_contains "$res" "== end" "$tag: the batch script continues after the failed step"
  assert_eq "$(failed_field "$id")" "0" "$tag: qacct failed is 0, the job was not deleted"
  rm -f "$job"
done
finish
