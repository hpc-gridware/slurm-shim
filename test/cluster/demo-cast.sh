#!/bin/bash
# The script recorded for the README asciicast. It runs INSIDE the cluster as an
# ordinary user, so the viewer sees a real session against a real Open Cluster
# Scheduler cluster. Nothing is staged: every line is the real output of the
# real binary talking to the real scheduler.
set -u
P='\033[1;32m$\033[0m'; C='\033[1m'; D='\033[0;90m'; R='\033[0m'

say()  { printf "${D}# %s${R}\n" "$*"; sleep 1.2; }
show() { printf "${P} ${C}%s${R}\n" "$*"; sleep 0.6; }
run()  { show "$*"; eval "$*"; echo; sleep 1.2; }

say "A stock SLURM batch script. Nothing in it knows about Grid Engine."
run "cat train.sh"

say "But this cluster runs Open Cluster Scheduler. There is no slurmd anywhere."
run "qhost | awk '/^-/{next} {printf \"%-14s %-9s %4s %6s\\n\",\$1,\$2,\$3,\$7}'"

say "Submit it the way you would on SLURM."
# Submitted ONCE: the output is shown and the id parsed from the same run.
show "sbatch train.sh"
sub="$(sbatch train.sh 2>&1)"; printf '%s\n' "$sub"; echo
jid="$(printf '%s' "$sub" | awk '{print $NF}')"
sleep 1.2

sleep 3
# Deliberately -o with real fields. squeue's DEFAULT format has three columns
# the shim cannot populate from `qstat -xml` -- TIME is always 0:00, NODES
# always 1, NODELIST only the master's host (internal/cli/squeue/format.go).
# Putting those in a demo would present placeholders as data. The README states
# the limitation; the demo shows what actually works.
say "squeue sees it, because squeue is the shim reading qstat."
run "squeue -o '%i %P %j %u %t'"

printf "${D}# waiting for it to finish...${R}\n"
for _ in $(seq 1 90); do squeue -h -j "$jid" 2>/dev/null | grep -q . || break; sleep 2; done
sleep 1

say "The SLURM_* environment was fabricated by the PE hook, and srun fanned six"
say "ranks across three nodes over qrsh -inherit tight integration."
run "cat slurm-$jid.out"

say "sacct reads the job back out of Grid Engine's accounting file."
run "sacct -j $jid --format=JobID,JobName,State,ExitCode,Elapsed"

say "One static binary. No SLURM installed, no daemons added."
run "sbatch --version"
sleep 2
