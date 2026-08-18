#!/usr/bin/env bash
# Capture the version-sensitive GE outputs the shim parses, into
# fixtures/<ocs-version>/, so cross-version differences are diffable and can feed
# back into the Go parsers (internal/gedata/*). Run against a live cluster.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster

ver="$(gridware 'qconf -help 2>&1 | head -1' | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
[ -n "$ver" ] || ver="unknown"
dir="$E2E_DIR/fixtures/$ver"
mkdir -p "$dir"
log "capturing OCS $ver fixtures -> ${dir#"$REPO_ROOT/"}"

ensure_gpu_complex

# Static config the launcher/preflight and GPU discovery depend on.
gridware "qconf -sc"                > "$dir/qconf-sc.txt"          # complexes (RSMAP row)
gridware "qconf -sp make"           > "$dir/qconf-sp-make.txt"     # the 'make' PE
gridware "qconf -se ocs-worker1"    > "$dir/qconf-se-worker1.txt"  # exechost complex_values
gridware "qstat -xml -f"            > "$dir/qstat-xml-f.xml"       # queue/host states

# A live GPU job so qstat can show the granted RSMAP (only visible while active).
gjob="$(mktemp)"; trap 'rm -f "$gjob"' EXIT
cat >"$gjob" <<'EOF'
#!/bin/bash
sleep 30
EOF
remote=/home/gridware/e2e-capture-gpu.sh
put_job "$gjob" "$remote"
id="$(gridware "qsub -terse -pe make 2 -l ${GPU_COMPLEX}=1 -q all.q@ocs-worker1 -o /dev/null -j y '$remote'")"
id="${id%%.*}"
if [ -n "$id" ]; then
  # Wait until it is running so the grant is populated.
  for _ in $(seq 1 30); do
    st="$(gridware "qstat -j '$id' 2>/dev/null | grep -c usage" || echo 0)"
    [ "${st:-0}" != 0 ] && break
    sleep 2
  done
  gridware "qstat -xml -j '$id'" > "$dir/qstat-xml-j.xml"   # granted RSMAP (GRU_*)
  gridware "qstat -j '$id'"      > "$dir/qstat-j.txt"        # plain resource_map line
  gridware "qdel '$id' >/dev/null 2>&1 || true"
  log "captured granted-RSMAP fixtures from job $id"
else
  log "warning: could not launch the capture GPU job; RSMAP fixtures skipped"
fi

# Best-effort qrsh rejection signatures (SI-09 job-race / SI-55 over-slot). These
# are hard to force deterministically; capture whatever stderr we can elicit.
{
  echo "# qrsh -inherit against a non-existent job (job-race-shaped stderr):"
  gridware "qrsh -inherit -nostdin nonexistent-host /bin/true 2>&1 | head -5 || true"
} > "$dir/qrsh-rejections.txt" 2>&1

log "OCS $ver fixtures written under ${dir#"$REPO_ROOT/"}"
