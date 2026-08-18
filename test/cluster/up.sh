#!/usr/bin/env bash
# Bring up the OCS cluster (from quickinstall, unmodified) and install slurm-shim.
#   up.sh [--gpu]
# Honors OCS_VERSION (default: 9.1.4) and QUICKINSTALL_REF.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

qi_ensure
log "starting OCS cluster (OCS_VERSION=$OCS_VERSION)"
compose up -d --build
wait_ready
"$CLUSTER_DIR/install-shim.sh" "$@"

cat >&2 <<EOF

  Cluster + slurm-shim are ready (OCS $(gridware 'echo $SGE_CLUSTER_NAME' >/dev/null 2>&1; docker exec ocs-master bash -lc 'qconf -help 2>&1 | head -1' 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || echo '?')).
  Try:   make demo        # multi-node srun fan-out
         make demo-gpu     # per-rank CUDA_VISIBLE_DEVICES (needs 'up --gpu')
  Shell: docker exec -it -u gridware ocs-master bash
  Down:  make cluster-down            (add ARGS=-v to also wipe the OCS install)
EOF
