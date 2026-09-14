#!/bin/bash
# The script recorded for the README asciicast. It runs INSIDE the cluster as an
# ordinary user, so the viewer sees a real session against a real Open Cluster
# Scheduler cluster. Nothing is staged: every line is the real output of the
# real binary talking to the real scheduler.
#
# Paced for a first-time viewer: commands are typed, and each result stays on
# screen long enough to read. The idle-time limits below must stay above the
# longest pause here (4s), or the recorder cuts the reading time back out.
#
# Recorded from the repo root, with `make cluster-up` running and this script
# and jobs/train.sh copied to the gridware home:
#
#   asciinema rec docs/assets/slurm-shim-demo.cast --overwrite --idle-time-limit 4.5 \
#     --command "docker exec -t -u gridware -w /home/gridware ocs-master bash -l demo-cast.sh"
#   agg --theme asciinema --font-size 16 --idle-time-limit 4.5 \
#     docs/assets/slurm-shim-demo.cast docs/assets/slurm-shim-demo.gif
set -u
P='\033[1;32m$\033[0m'; C='\033[1m'; D='\033[0;90m'; R='\033[0m'

say() { printf "${D}# %s${R}\n" "$*"; sleep 1.6; }

# typed <command>: print the command as if typed, then pause before its output.
typed() {
  local s="$*" i
  printf "${P} ${C}"
  for ((i = 0; i < ${#s}; i++)); do
    printf '%s' "${s:i:1}"
    sleep 0.035
  done
  printf "${R}\n"
  sleep 0.6
}

# run <command> [hold-seconds]: type it, run it, keep the result on screen.
run() {
  typed "$1"
  eval "$1"
  echo
  sleep "${2:-2.2}"
}

say "A stock SLURM batch script. Nothing in it knows about Grid Engine."
run "cat train.sh" 3

say "This cluster runs Open Cluster Scheduler. SLURM is not installed."
run "qconf -help | head -1" 1.2
run "qconf -sel" 2.5

say "Submit it exactly as on SLURM."
# Submitted ONCE: the output is shown and the id parsed from the same run.
typed "sbatch train.sh"
sub="$(sbatch train.sh 2>&1)"; printf '%s\n\n' "$sub"
jid="$(printf '%s' "$sub" | awk '{print $NF}')"
sleep 2.2

# Deliberately -o with real fields. squeue's DEFAULT format has three columns
# the shim cannot populate from `qstat -xml` -- TIME is always 0:00, NODES
# always 1, NODELIST only the master's host (internal/cli/squeue/format.go).
# Putting those in a demo would present placeholders as data.
say "squeue shows it running."
run "squeue -o '%i %j %u %t'" 2.5

printf "${D}# waiting for the job to finish...${R}\n"
for _ in $(seq 1 90); do squeue -h -j "$jid" 2>/dev/null | grep -q . || break; sleep 2; done

say "The job got the SLURM environment it expects, and srun started"
say "two ranks on each of the three nodes."
run "cat slurm-$jid.out" 4

say "sacct reads the finished job back from Grid Engine."
run "sacct -j $jid --format=JobID,JobName,State,ExitCode,Elapsed" 3

say "One static binary, no daemons added."
run "sbatch --version" 3.5
