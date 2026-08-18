#!/bin/bash
#SBATCH --job-name=ray-smoke
#SBATCH --partition=batch
#SBATCH --nodes=3
#SBATCH --ntasks-per-node=1
#SBATCH --output=ray-smoke-%j.out
# CPU-only, GPU-free smoke test of the Ray integration: bring a real multi-node
# Ray cluster up on the shim (srun-bootstrap) and assert it formed across the
# nodes and ran tasks on more than one host. Light on purpose (2 cpus/node).
#
# `ray` and `python` must resolve on every node (e.g. a shared venv on a shared
# filesystem). Override with RAY_BIN / PYTHON_BIN if they are not on PATH.
set -uo pipefail

RAY="${RAY_BIN:-ray}"
PY="${PYTHON_BIN:-python3}"
PORT="${RAY_PORT:-6379}"

. /opt/slurm-shim/etc/slurm-shim-source-hook.sh

mapfile -t NODES < <(scontrol show hostnames)
HEAD="${NODES[0]}"
HEAD_IP="$(getent hosts "$HEAD" | awk 'NR==1{print $1}')"   # NR==1: getent may return >1 line
export RAY_ADDRESS="$HEAD_IP:$PORT"
echo "[ray] head=$HEAD ($HEAD_IP:$PORT)  nodes=${#NODES[@]}: ${NODES[*]}"

# Head on node 0; workers on the rest. --block keeps each Ray process (and its
# srun step) alive; run them in the background so the driver can proceed.
srun --nodes=1 --ntasks=1 -w "$HEAD" \
  "$RAY" start --head --node-ip-address="$HEAD_IP" --port="$PORT" \
    --num-cpus=2 --num-gpus=0 --include-dashboard=false --block &
for w in "${NODES[@]:1}"; do
  srun --nodes=1 --ntasks=1 -w "$w" \
    "$RAY" start --address="$HEAD_IP:$PORT" --num-cpus=2 --num-gpus=0 --block &
done

# Wait until every node has registered with the head.
count_nodes() { "$PY" - <<'PY' 2>/dev/null
import ray; ray.init(address="auto", logging_level="ERROR")
print(sum(1 for n in ray.nodes() if n["Alive"]))
PY
}
for _ in $(seq 1 60); do
  n="$(count_nodes)"; n="${n:-0}"; echo "[ray] alive nodes: $n/${#NODES[@]}"
  [ "$n" -ge "${#NODES[@]}" ] && break
  sleep 3
done

# Driver: assert the cluster and that tasks run across >1 host.
"$PY" - <<'PY'
import ray, socket, collections, sys
ray.init(address="auto", logging_level="ERROR")
nodes = [n for n in ray.nodes() if n["Alive"]]
print(f"[driver] alive ray nodes: {len(nodes)}")
print(f"[driver] cluster CPU: {ray.cluster_resources().get('CPU')}")
@ray.remote
def where():
    import socket, time; time.sleep(0.05); return socket.gethostname()
hosts = collections.Counter(ray.get([where.remote() for _ in range(40)]))
print(f"[driver] task placement across hosts: {dict(hosts)}")
ok = len(nodes) >= len({*hosts}) and len(hosts) >= 2 and len(nodes) >= 2
print("[driver] RAY MULTI-NODE OK" if ok else "[driver] RAY MULTI-NODE FAILED")
sys.exit(0 if ok else 1)
PY
rc=$?

# Teardown: stop the head, drop the worker srun steps (the shim's srun kill path
# stops the remote Ray processes). GE's qdel is the backstop.
"$RAY" stop >/dev/null 2>&1 || true
kill $(jobs -p) 2>/dev/null || true
echo "[ray] driver rc=$rc"
exit $rc
