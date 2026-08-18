#!/usr/bin/env bash
# Check: the shim's sbatch translates #SBATCH directives to a qsub submission and
# prints the SLURM-format "Submitted batch job <id>" line, and the job runs.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-lib.sh"
require_cluster
log "30_sbatch: #SBATCH -> qsub -terse -> 'Submitted batch job'"

job="$(mktemp)"
trap 'rm -f "$job"' EXIT
cat >"$job" <<'EOF'
#!/bin/bash
#SBATCH --partition=batch
#SBATCH --nodes=1
#SBATCH --ntasks-per-node=1
#SBATCH --job-name=e2e-sbatch
echo "hello-from-sbatch on $(hostname)"
EOF

remote=/home/gridware/e2e-30-sbatch.sh
out=/home/gridware/e2e-30-sbatch.out
put_job "$job" "$remote"
gridware "rm -f '$out'"

raw="$(gridware "sbatch --output='$out' '$remote'")"
assert_contains "$raw" "Submitted batch job" "sbatch prints the SLURM-format submit line"
id="$(printf '%s' "$raw" | awk '/Submitted batch job/{print $NF}')"
if [ -n "$id" ]; then pass "submit line carries a job id ($id)"; else fail "no job id in submit line"; fi

res="$(jobout "$id" "$out")"
assert_contains "$res" "hello-from-sbatch" "the translated batch job ran and wrote output"
finish
