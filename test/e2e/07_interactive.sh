#!/usr/bin/env bash
# Check: srun --pty outside an allocation becomes an interactive qrsh session.
# The shim translates the SLURM flags and execs qrsh; qrsh owns the pty, the
# allocation wait, signals and the exit status. Verified design facts are in
# docs/plans/2026-09-04-feat-interactive-srun-pty-via-qrsh-plan.md.
#
# A pty is provided by script(1) inside the container -- but note the session
# itself does not need the CLIENT to have a tty (the remote pty is allocated by
# -pty y regardless); script(1) here just exercises the realistic path.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "07_interactive: srun --pty outside an allocation -> qrsh session"

# ptyrun <script-body> -- run a body as gridware under a real pty, return stdout.
ptyrun() {
  local body="$1" f="$JOB_HOME/e2e-07.sh"
  printf '#!/bin/bash\n%s\n' "$body" > /tmp/e2e-07.sh
  put_job /tmp/e2e-07.sh "$f"
  # Invoke via bash so the copied file needs no execute bit, under script(1) so
  # the session gets a real pty.
  node_sh "$MASTER" "script -qec 'su - $JOB_USER -c \"bash $f\"' /dev/null" 2>&1 | tr -d '\r'
}

# (1) A real session: pty, the SLURM_* environment, and the invocation dir.
out="$(ptyrun 'cd "$HOME" && srun --pty -p batch -c 2 bash -c "echo GOT tty=\$(tty) JOB=\$SLURM_JOB_ID NN=\$SLURM_NNODES PART=\$SLURM_JOB_PARTITION pwd=\$(pwd)"')"
assert_contains "$out" "tty=/dev/pts/" "the session runs on a real pty"
assert_contains "$out" "JOB=" "SLURM_JOB_ID is set in the session"
assert_contains "$out" "pwd=$JOB_HOME" "the session starts in the invocation dir (-cwd)"

# (2) Exit status propagates through qrsh.
out="$(ptyrun 'srun --pty -p batch bash -c "exit 7"; echo "RC=$?"')"
assert_contains "$out" "RC=7" "the shell's exit status is srun's exit status"

# (3) Quoted argv survives intact (no re-shelling).
out="$(ptyrun 'srun --pty -p batch bash -c "for a in \"\$@\"; do echo ARG=[\$a]; done" _ "a b" X' | tr -d ' ')"
# collapse spaces only for the match on the spaced arg:
raw="$(ptyrun 'srun --pty -p batch bash -c "for a in \"\$@\"; do echo ARG=[\$a]; done" _ "a b" X')"
assert_contains "$raw" "ARG=[a b]" "a quoted argument with a space is preserved"

# (4) --account round-trips to SLURM_JOB_ACCOUNT.
out="$(ptyrun 'srun --pty -p batch -A e2eacct bash -c "echo ACCT=\$SLURM_JOB_ACCOUNT"')"
assert_contains "$out" "ACCT=e2eacct" "--account reaches SLURM_JOB_ACCOUNT via -A/SGE_ACCOUNT"

# (5) 2-node --pty session, then srun steps fan out from inside it.
out="$(ptyrun 'srun --pty -p batch -N 2 --ntasks-per-node=1 bash -c "echo OUTER NN=\$SLURM_NNODES; srun hostname 2>/dev/null | sort | tr \"\\n\" \" \"; echo"')"
assert_contains "$out" "OUTER NN=2" "the interactive session spans two nodes"
# Match against THIS cluster's nodes rather than a hardcoded container pattern.
# A grep that finds nothing exits 1, and under `set -e` that killed the check
# outright -- silently, right after the previous assertion passed.
hosts=""
for _h in "${NODES[@]}"; do
  case "$out" in *"$_h"*) hosts="$hosts $_h" ;; esac
done
n="$(printf '%s' "$hosts" | tr ' ' '\n' | grep -c . || true)"
assert_eq "$n" "2" "inner srun fanned out to both allocated nodes ($hosts)"

# (6) Dry run: prints the qrsh line, creates no job.
# Compare the SET of job ids, not a line count. A count changes whenever an
# unrelated job finishes between the two samples, which fails the check for a
# reason that has nothing to do with the dry run.
qids() { gridware "qstat -u '*' 2>/dev/null | awk 'NR>2 {print \$1}' | sort" 2>/dev/null || true; }
before_ids="$(qids)"
out="$(gridware 'SLURM_SHIM_DRY_RUN=1 srun --pty -p batch -c 2 bash 2>&1')"
assert_contains "$out" "-now no -pty y -cwd -q all.q -pe make 2" "dry run prints the resolved qrsh line"
after_ids="$(qids)"
new_ids="$(comm -13 <(printf '%s\n' "$before_ids") <(printf '%s\n' "$after_ids") | grep -c . || true)"
assert_eq "$new_ids" "0" "dry run created no job"

# (7) QRSH_WRAPPER is scrubbed (cannot hijack the session).
out="$(ptyrun 'QRSH_WRAPPER=/bin/echo srun --pty -p batch bash -c "echo REAL JOB=\$SLURM_JOB_ID"')"
assert_contains "$out" "REAL JOB=" "QRSH_WRAPPER is scrubbed; the real command runs"

# (8) srun without --pty outside an allocation still rejects (unchanged).
out="$(gridware 'srun hostname 2>&1 || true')"
assert_contains "$out" "not inside a slurm-shim allocation" "non-pty standalone srun still rejects"

gridware "rm -f $JOB_HOME/e2e-07.sh"
finish
