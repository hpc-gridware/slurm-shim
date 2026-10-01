#!/usr/bin/env bash
# Check: torchrun's elastic agent takes the hot spare and training resumes.
#
# A 2-node gloo run with --x-spares=1 checkpoints every step. The active slave
# is partitioned off the network mid-run: srun relaunches its torchrun agent on
# the spare, the surviving agent re-enters c10d rendezvous, both restart their
# workers, and train.py resumes from the checkpoint and finishes. The Grid
# Engine job never leaves RUNNING.
#
# Needs a torch venv on the shared home (TORCH_VENV, default ~/torchenv; CPU
# torch is enough), plus what 33_spares needs: docker backend, 3 exec nodes,
# OCS 9.1.5+.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "34_spares_torchrun: torchrun re-forms its group on the spare and finishes"

venv=${TORCH_VENV:-$JOB_HOME/torchenv}
require_spares_cluster
if ! gridware "[ -x $venv/bin/torchrun ]"; then
  skip "no torch venv at $venv (python3 -m venv $venv && $venv/bin/pip install torch importlib_metadata)"
  finish
fi

target="" id="" job="" train=""
cleanup() {
  heal
  [ -n "$id" ] && gridware "qdel $id" >/dev/null 2>&1 || true
  rm -f "$job" "$train"
}
trap cleanup EXIT

d=$JOB_HOME/e2e-34
gridware "rm -rf $d && mkdir -p $d"
cfg=$d/config.yaml
fast_liveness_config "$cfg"
gridware "echo '-masterq all.q@$MASTER' > $d/.sge_request"

# Every rank all-reduces once per step; rank 0 checkpoints the step. A short
# gloo timeout lets the survivor notice the lost peer fast.
train="$(mktemp)"
cat >"$train" <<'PY'
import datetime, os, socket, time
import torch, torch.distributed as dist
ckpt = os.path.join(os.environ["CKPT_DIR"], "step")
dist.init_process_group("gloo", timeout=datetime.timedelta(seconds=20))
rank, world = dist.get_rank(), dist.get_world_size()
step = int(open(ckpt).read()) if os.path.exists(ckpt) else 0
print(f"rank {rank}/{world} on {socket.gethostname()} starts at step {step} "
      f"(restart {os.environ.get('TORCHELASTIC_RESTART_COUNT')})", flush=True)
if rank == 1:
    open(os.path.join(os.environ["CKPT_DIR"], f"host.{step}"), "w").write(socket.gethostname())
while step < 25:
    t = torch.ones(1)
    dist.all_reduce(t)
    assert t.item() == world
    step += 1
    if rank == 0:
        open(ckpt + ".tmp", "w").write(str(step)); os.replace(ckpt + ".tmp", ckpt)
    time.sleep(1)
print(f"rank {rank} DONE step={step} world={world} on {socket.gethostname()}", flush=True)
dist.destroy_process_group()
PY
put_job "$train" "$d/train.py"

job="$(mktemp)"
cat >"$job" <<EOF
#!/bin/bash
#SBATCH --nodes=2 --ntasks-per-node=1
#SHIM --x-spares=1
export SLURM_SHIM_CONFIG=$cfg CKPT_DIR=$d
MASTER_ADDR=\$(scontrol show hostnames \$SLURM_JOB_NODELIST | head -1)
echo "SPARES=\$SLURM_X_SPARE_NODELIST"
srun $venv/bin/torchrun --nnodes=2 --nproc-per-node=1 --max-restarts=3 \\
  --rdzv-backend=c10d --rdzv-endpoint=\$MASTER_ADDR:29511 --rdzv-id=\$SLURM_JOB_ID \\
  $d/train.py
echo "SRUN_RC=\$?"
EOF
put_job "$job" "$d/job.sh"
id="$(gridware "cd $d && sbatch --output=$d/out job.sh" | awk '/Submitted batch job/{print $NF}')"
if [ -z "$id" ]; then
  fail "sbatch returned no job id"
  finish
fi

# Partition rank 1's host once training is a few steps in.
for _ in $(seq 1 90); do
  s="$(gridware "cat $d/step 2>/dev/null" || true)"
  [ "${s:-0}" -ge 5 ] 2>/dev/null && break
  sleep 2
done
target="$(gridware "cat $d/host.0 2>/dev/null" || true)"
if [ -z "$target" ] || [ "$target" = "$MASTER" ]; then
  fail "training did not get going on a slave (rank 1 host '${target}', step ${s:-none})"
  gridware "cat $d/out" >&2 || true
  target=""
  finish
fi
lost=$target
partition "$target" " at step $s"

for _ in $(seq 1 120); do
  gridware "grep -q SRUN_RC= $d/out" 2>/dev/null && break
  sleep 2
done
heal

res="$(jobout "$id" "$d/out")"
spare="$(printf '%s\n' "$res" | sed -n 's/^SPARES=//p')"
assert_contains "$res" "node $lost lost; tasks 1 relaunched on $spare (1/1 spares used)" "srun moved the torchrun agent to the spare"
if printf '%s\n' "$res" | grep -Eq "rank 1/2 on $spare starts at step [1-9]"; then
  pass "a restarted worker on the spare resumed from the checkpoint"
else
  fail "no resumed worker on the spare: $(printf '%s\n' "$res" | grep 'starts at' | tr '\n' '|')"
fi
assert_contains "$res" "rank 0 DONE step=25 world=2" "training finished at full size"
assert_contains "$res" "rank 1 DONE step=25 world=2 on $spare" "the spare's worker finished it with rank 0"
assert_contains "$res" "SRUN_RC=0" "the step succeeded"
assert_eq "$(failed_field "$id")" "0" "qacct failed is 0: the job never left RUNNING"
id=""
if [ "$(printf '%s\n' "$res" | grep -c 'srun: warning: torchrun')" = 0 ]; then
  pass "a well-configured torchrun step gets no advice"
else
  fail "srun warned about a well-configured torchrun: $(printf '%s\n' "$res" | grep 'srun: warning: torchrun')"
fi

wait_host_up "$lost"
gridware "rm -rf $d"
finish
