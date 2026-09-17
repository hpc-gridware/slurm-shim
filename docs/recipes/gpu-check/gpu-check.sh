#!/bin/bash
#SBATCH --job-name=gpu-check
#SBATCH --partition=gpu
#SBATCH --nodes=4
#SBATCH --ntasks-per-node=2
#SBATCH --gpus-per-node=2
#SBATCH --output=gpu-check-%j.out
# Does every rank get the GPUs Grid Engine granted? NVIDIA only. The job exits
# non-zero when any check fails.
# One task per GPU: keep --ntasks-per-node equal to --gpus-per-node.
# (Comments stay off the #SBATCH lines: slurm-shim up to v0.6.0 reads an inline
# comment there as arguments and drops the directives that follow.)
#
# One RANK line per task:
#   given  - CUDA_VISIBLE_DEVICES as the shim set it from the RSMAP grant: the
#            devices CUDA frameworks will use.
#   opened - the GPUs the task can reach, or `error` if nvidia-smi failed.
#            nvidia-smi ignores CUDA_VISIBLE_DEVICES, so under gpu.isolation:
#            shim this is every GPU on the node.
#   ok     - 1 when every device in given is reachable (a UUID must be listed
#            in opened, a numeric id must index into it). Under gpu.isolation:
#            cgroup the shim sets no mask; then the task must reach exactly the
#            node's granted number of GPUs.
# The BATCH line shows the batch script's own mask and its address-space limit,
# which must be `unlimited` (an h_vmem limit kills CUDA at start-up).
# The JOB line needs every task to report ok=1, that limit to be unlimited, and
# counts distinct (host, device) pairs in given: the job must hold nodes x GPUs
# per node of them, none granted twice.
ulimit_v=$(ulimit -v)
echo "BATCH host=$(hostname -s) given=[${CUDA_VISIBLE_DEVICES:-unset}] ulimit_v=$ulimit_v"
ranks=$(srun bash -c '
  if smi=$(nvidia-smi --query-gpu=uuid --format=csv,noheader 2>/dev/null); then
    vis=$(printf "%s\n" "$smi" | grep "^GPU-" | paste -sd, -)
  else
    vis=error
  fi
  n_open=$(printf "%s\n" "$vis" | tr , "\n" | grep -c "^GPU-")
  ok=1
  if [ "$vis" = error ]; then
    ok=0
  elif [ -n "${CUDA_VISIBLE_DEVICES:-}" ]; then
    for id in $(printf "%s" "$CUDA_VISIBLE_DEVICES" | tr , " "); do
      case "$id" in
        GPU-*) case ",$vis," in *",$id,"*) ;; *) ok=0 ;; esac ;;
        *) [ "$id" -lt "$n_open" ] 2>/dev/null || ok=0 ;;
      esac
    done
  elif [ "${SLURM_GPUS_ON_NODE:-0}" -gt 0 ]; then
    [ "$n_open" -eq "$SLURM_GPUS_ON_NODE" ] || ok=0
  else
    ok=0
  fi
  echo "RANK host=$(hostname -s) localid=$SLURM_LOCALID procid=$SLURM_PROCID given=[${CUDA_VISIBLE_DEVICES:-unset}] rocr=[${ROCR_VISIBLE_DEVICES:-unset}] opened=[$vis] n_open=$n_open ok=$ok"
')
rc=$?
printf '%s\n' "$ranks"

# The tasks always exit 0 so that every one of them reports; the verdict is
# made here. A failing task would make srun stop the others (kill-on-bad-exit).
seen=$(printf '%s\n' "$ranks" | grep -c '^RANK ')
bad=$(printf '%s\n' "$ranks" | grep -c '^RANK .* ok=0$')
want=$((SLURM_NNODES * ${SLURM_GPUS_PER_NODE:-0}))
got=$(printf '%s\n' "$ranks" |
  sed -n 's/^RANK host=\([^ ]*\) .* given=\[\([^]]*\)\].*/\1 \2/p' |
  while read -r host ids; do
    [ "$ids" = unset ] && continue
    for id in ${ids//,/ }; do echo "$host $id"; done
  done | sort -u | wc -l | tr -d ' ')
jobok=1
if [ "$rc" -ne 0 ] || [ "$bad" -ne 0 ] || [ "$seen" -ne "${SLURM_NTASKS:-0}" ] ||
   [ "$ulimit_v" != unlimited ]; then
  jobok=0
fi
# No mask at all (cgroup) leaves no devices to count; the RANK lines cover it.
if [ "$got" -gt 0 ] && [ "$got" -ne "$want" ]; then
  jobok=0
fi
echo "JOB ranks=$seen/${SLURM_NTASKS:-0} failed=$bad devices=$got/$want ok=$jobok"
[ "$jobok" = 1 ]
