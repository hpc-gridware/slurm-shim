#!/usr/bin/env bash
# Shared helpers and assertions for the e2e checks. Each NN_*.sh sources this,
# runs its assertions, and ends with `finish` (exit non-zero if any failed).
#
# The cluster helpers (gridware/manager/require_cluster/MASTER/NODES/...) come
# from the Phase 1 cluster lib, reused verbatim so there is one source of truth.
set -uo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../cluster/lib.sh
source "$E2E_DIR/../cluster/lib.sh"

E2E_PASS=0
E2E_FAIL=0

pass() { E2E_PASS=$((E2E_PASS + 1)); printf '  \033[1;32mok\033[0m   %s\n' "$*"; }
fail() { E2E_FAIL=$((E2E_FAIL + 1)); printf '  \033[1;31mFAIL\033[0m %s\n' "$*" >&2; }
skip() { printf '  \033[1;33mskip\033[0m %s\n' "$*"; }

# assert_contains <haystack> <needle> <label>
assert_contains() {
  case "$1" in
    *"$2"*) pass "$3" ;;
    *) fail "$3 -- missing '$2' in: $(printf '%s' "$1" | tr '\n' '|')" ;;
  esac
}

# assert_eq <got> <want> <label>
assert_eq() {
  if [ "$1" = "$2" ]; then pass "$3"; else fail "$3 -- want '$2' got '$1'"; fi
}

# finish exits with 1 if any assertion in this check failed.
finish() {
  if [ "$E2E_FAIL" -gt 0 ]; then
    printf '  -> %d passed, \033[1;31m%d failed\033[0m\n' "$E2E_PASS" "$E2E_FAIL" >&2
    exit 1
  fi
  printf '  -> %d passed\n' "$E2E_PASS"
  exit 0
}

# put_job <local-script> <remote-path> copies a job script to the master owned by
# the gridware user.
put_job() {
  docker cp "$1" "$MASTER:$2" >/dev/null
  docker exec "$MASTER" chown gridware:gridware "$2"
}

# jobout <jobid> <outfile> waits (bounded) for the job to leave the queue, then
# prints the output file. Empty if it never produced one.
jobout() {
  local id="$1" out="$2"
  for _ in $(seq 1 90); do
    gridware "squeue -h -j '$id' 2>/dev/null | grep -q ." || break
    sleep 2
  done
  gridware "cat '$out' 2>/dev/null"
}

# sbatch_submit <remote-script> <outfile> [extra sbatch args...] submits through
# the shim's sbatch and echoes the numeric job id (or empty on failure).
sbatch_submit() {
  local script="$1" out="$2"
  shift 2
  gridware "rm -f '$out'"
  gridware "sbatch $* --output='$out' '$script'" | awk '/Submitted batch job/{print $NF}'
}
