#!/usr/bin/env bash
# Check: commands that block until their work is done, outside any allocation.
#   - srun without --pty creates an allocation, runs the command as a step over
#     it, and returns the step's exit status; -I starts at once or fails.
#   - sbatch --wait returns the job's exit status (an array's highest).
#   - salloc runs one command (or a shell) on a new allocation; srun inside it
#     launches steps across the allocation.
#   - A blocking srun killed with SIGTERM ends its job; an interrupted
#     sbatch --wait stops waiting and leaves the job queued.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "22_blocking: srun, salloc and sbatch --wait outside an allocation"

# Every command gets stdin from /dev/null: the harness feeds the shell through
# stdin, and salloc forwards its stdin to the command it runs.

# -- srun as a step on a new allocation ----------------------------------------
res="$(gridware "timeout -s INT 180 srun -N 2 --ntasks-per-node=1 -l hostname </dev/null 2>&1; echo rc=\$?")"
assert_contains "$res" "rc=0" "srun -N 2 -l hostname succeeds outside an allocation"
assert_contains "$res" "0: " "task 0 labelled"
assert_contains "$res" "1: " "task 1 labelled"
hosts="$(printf '%s\n' "$res" | sed -n 's/^[01]: //p' | sort -u | wc -l | tr -d ' ')"
assert_eq "$hosts" "2" "the two tasks ran on two hosts"
case "$res" in *warning*) fail "srun printed a warning: $(printf '%s' "$res" | grep warning | head -1)" ;; *) pass "no warnings" ;; esac

res="$(gridware "timeout -s INT 180 srun sh -c 'exit 3' </dev/null 2>/dev/null; echo rc=\$?")"
assert_contains "$res" "rc=3" "srun returns the step's exit status"

res="$(gridware "timeout -s INT 180 srun -I hostname </dev/null 2>&1; echo rc=\$?")"
assert_contains "$res" "rc=0" "srun -I starts at once on an idle cluster"

# -- sbatch --wait --------------------------------------------------------------
res="$(gridware "timeout -s INT 300 sbatch --wait --wrap 'exit 3' </dev/null; echo rc=\$?")"
assert_contains "$res" "Submitted batch job" "sbatch --wait prints the job id first"
assert_contains "$res" "rc=3" "sbatch --wait exits with the job's exit status"

res="$(gridware "timeout -s INT 300 sbatch --wait --array=0-2 --wrap 'exit \$SLURM_ARRAY_TASK_ID' </dev/null; echo rc=\$?")"
assert_contains "$res" "rc=2" "sbatch --wait on an array exits with the highest task status"

# -- salloc ---------------------------------------------------------------------
res="$(gridware "timeout -s INT 180 salloc -N 1 hostname </dev/null 2>&1; echo rc=\$?")"
assert_contains "$res" "rc=0" "salloc runs a command on a new allocation"
assert_contains "$res" "ocs-" "salloc's command ran on a cluster host"

res="$(gridware "timeout -s INT 180 salloc -N 2 --ntasks-per-node=1 srun -l hostname </dev/null 2>&1; echo rc=\$?")"
assert_contains "$res" "rc=0" "srun inside salloc launches a step"
assert_contains "$res" "1: " "the step spans the salloc allocation"

# -- signals --------------------------------------------------------------------
# SIGTERM to a running blocking srun ends its job and returns 128+15.
# gridware runs "cd && <command>": the leading ':' keeps the & below from
# backgrounding that whole list, so \$! is srun itself.
res="$(gridware ":
srun -J e2e22term sleep 300 </dev/null >/dev/null 2>&1 &
p=\$!
for _ in \$(seq 1 60); do
  qstat -u \$USER | awk '\$3==\"e2e22term\"{s=\$5} END{exit s==\"r\"?0:1}' && break
  sleep 2
done
kill -TERM \$p; wait \$p; echo rc=\$?
# The job passes through dr (deleting) for a few seconds before it leaves.
for _ in \$(seq 1 30); do qstat -u \$USER | grep -q e2e22term || break; sleep 1; done
qstat -u \$USER | grep -c e2e22term || true
")"
assert_contains "$res" "rc=143" "a blocking srun killed with SIGTERM exits 128+15"
assert_eq "$(printf '%s\n' "$res" | tail -1)" "0" "its job is gone"

# An interrupted sbatch --wait stops waiting; the job stays.
res="$(gridware "
out=\$(mktemp)
sbatch --wait -J e2e22wait --wrap 'sleep 120' </dev/null >\$out 2>&1 &
p=\$!
for _ in \$(seq 1 30); do grep -q 'Submitted batch job' \$out && break; sleep 1; done
kill -INT \$p; wait \$p; echo rc=\$?
id=\$(sed -n 's/Submitted batch job //p' \$out)
qstat -j \$id >/dev/null 2>&1 && echo job=present
qdel \$id >/dev/null 2>&1
cat \$out; rm -f \$out
")"
assert_contains "$res" "rc=130" "an interrupted sbatch --wait exits 128+2"
assert_contains "$res" "job=present" "the job keeps its place"
assert_contains "$res" "keeps running" "sbatch says the job keeps running"

finish
