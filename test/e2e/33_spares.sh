#!/usr/bin/env bash
# Check: a hot spare replaces a lost node and the step carries on.
#
# sbatch --x-spares=1 on a 2-node request grants 3 hosts; the fabricator keeps
# the last one idle as a spare (SLURM_NNODES=2, SLURM_X_SPARE_NODELIST). The
# active slave is then partitioned off the network. srun's channel to it times
# out (kernel liveness), and the elastic step relaunches that node's task on the
# spare through qrsh -inherit: same rank, same node index. The step finishes
# with exit 0, the next step runs on the replacement, scontrol shows the swap
# inside and outside the job, and the Grid Engine job never leaves RUNNING.
#
# Docker backend only (the partition is `docker network disconnect`), 3 exec
# nodes (N=2 + 1 spare), OCS 9.1.5+ (spares need qsub -par).
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "33_spares: a hot spare replaces a partitioned node; the step succeeds"

require_spares_cluster

# A spare needs a node to stand in for: one node plus spares is refused at
# submit, with the reason, and nothing is queued.
one="$(gridware "sbatch --nodes=1 --x-spares=1 --wrap true 2>&1; echo rc=\$?")"
assert_contains "$one" "at least 2 nodes" "sbatch refuses --x-spares with --nodes=1, saying why"
assert_contains "$one" "rc=1" "the refused submit exits 1"

target="" id="" job=""
cleanup() {
  heal
  [ -n "$id" ] && gridware "qdel $id" >/dev/null 2>&1 || true
  rm -f "$job"
}
trap cleanup EXIT

d=$JOB_HOME/e2e-33
gridware "rm -rf $d && mkdir -p $d"
# Short liveness timers via the job's config copy: the stale stepper gives up
# after 10s, srun declares the node lost after 15s.
cfg=$d/config.yaml
fast_liveness_config "$cfg"
# Pin the job's master to the qmaster host, so the partitioned node (the active
# slave) is a plain exec host: qrsh -inherit to the spare needs qmaster.
gridware "echo '-masterq all.q@$MASTER' > $d/.sge_request"

job="$(mktemp)"
cat >"$job" <<EOF
#!/bin/bash
#SBATCH --nodes=2 --ntasks-per-node=1
#SHIM --x-spares=1
export SLURM_SHIM_CONFIG=$cfg
echo "NNODES=\$SLURM_NNODES SPARES=\$SLURM_X_SPARE_NODELIST NODELIST=\$SLURM_JOB_NODELIST"
srun --x-elastic=on bash -c '
  echo "rank \$SLURM_PROCID on \$(hostname)"
  m=$d/started.\$SLURM_PROCID
  if [ -e "\$m" ]; then touch $d/relaunched; exit 0; fi   # the relaunch: done
  touch "\$m"; hostname > $d/host.\$SLURM_PROCID
  if [ "\$SLURM_PROCID" = 0 ]; then
    while [ ! -e $d/relaunched ]; do sleep 1; done; exit 0
  fi
  exec sleep 342'
echo "SRUN_RC=\$?"
echo "STEP2=\$(srun hostname | sort | tr '\n' ' ')"
scontrol show job \$SLURM_JOB_ID | tr ' ' '\n' | grep -E '^(SpareNodes|SwappedNodes)='
sleep 20
echo SCRIPT_END
EOF
put_job "$job" "$d/job.sh"
id="$(gridware "cd $d && sbatch --output=$d/out job.sh" | awk '/Submitted batch job/{print $NF}')"
if [ -z "$id" ]; then
  fail "sbatch returned no job id"
  finish
fi

# Rank 1 runs on the active slave; partition it once both ranks run.
for _ in $(seq 1 60); do
  gridware "[ -e $d/host.0 ] && [ -e $d/host.1 ]" && break
  sleep 2
done
target="$(gridware "cat $d/host.1 2>/dev/null" || true)"
if [ -z "$target" ] || [ "$target" = "$MASTER" ]; then
  fail "rank 1 did not start on a slave (got '${target}')"
  target=""
  finish
fi
lost=$target
# Outside the job, squeue counts the active nodes only, not the spare.
squeue_nodes="$(gridware "squeue -h -j $id -o %D" 2>/dev/null | tr -d ' ' || true)"
partition "$target"

for _ in $(seq 1 45); do
  gridware "grep -q SRUN_RC= $d/out" 2>/dev/null && break
  sleep 2
done
rank_left=running
for _ in $(seq 1 15); do
  rank_left="$(node_sh "$lost" "pgrep -f 'sleep 342' >/dev/null && echo running || echo gone" || true)"
  [ "$rank_left" = gone ] && break
  sleep 2
done
heal
# Outside the job (no layout here), scontrol reads Grid Engine: the swap srun
# recorded in the job context must show, and the node list name the spare.
outside="$(gridware "scontrol show job $id" 2>/dev/null || true)"

res="$(jobout "$id" "$d/out")"
spare="$(printf '%s\n' "$res" | sed -n 's/.* SPARES=\([^ ]*\) .*/\1/p')"
assert_contains "$res" "NNODES=2 SPARES=" "the job sees 2 nodes and a spare"
if [ -n "$spare" ] && [ "$spare" != "$lost" ] && [ "$spare" != "$MASTER" ]; then
  pass "the spare ($spare) is a third host, kept out of SLURM_JOB_NODELIST"
else
  fail "unexpected spare '$spare' (lost $lost, master $MASTER)"
fi
assert_contains "$res" "node $lost lost; tasks 1 relaunched on $spare (1/1 spares used)" "srun relaunched the lost node's task on the spare"
assert_contains "$res" "rank 1 on $spare" "rank 1 ran again on the spare"
assert_contains "$res" "SRUN_RC=0" "the step succeeded despite the lost node"
assert_contains "$res" "STEP2=" "a second step ran"
step2="$(printf '%s\n' "$res" | sed -n 's/^STEP2=//p')"
case " $step2 " in
  *" $lost "*) fail "the next step still ran on the lost node $lost ($step2)" ;;
  *" $spare "*) pass "the next step runs on the replacement ($step2)" ;;
  *) fail "the next step did not run on the replacement ($step2)" ;;
esac
assert_eq "$squeue_nodes" "2" "squeue shows the 2 active nodes, not the spare"
assert_contains "$res" "SwappedNodes=$lost" "scontrol inside the job shows the swap"
assert_contains "$outside" "SwappedNodes=$lost" "scontrol outside the job shows the swap"
nodelist="$(printf '%s\n' "$outside" | tr ' ' '\n' | sed -n 's/^NodeList=//p')"
case ",$nodelist," in
  *",$lost,"*) fail "scontrol outside the job still lists the lost node: NodeList=$nodelist" ;;
  *",$spare,"*) pass "scontrol outside the job lists the replacement (NodeList=$nodelist)" ;;
  *) fail "scontrol outside the job does not list the replacement: NodeList=$nodelist" ;;
esac
assert_eq "$rank_left" "gone" "the stale stepper on the partitioned host stopped its rank"
assert_contains "$res" "SCRIPT_END" "the batch script runs to its end"
assert_eq "$(failed_field "$id")" "0" "qacct failed is 0: the job never left RUNNING or died"
id=""

wait_host_up "$lost"
gridware "rm -rf $d"

finish
