#!/bin/sh
# Reference Open Cluster Scheduler load sensor for draining hosts that a
# hot-spare swap took out of a job (sbatch --x-spares). Unprivileged: a job
# user's srun marks a host by creating a file, and the host's queue instances
# go to alarm, so the scheduler stops sending new work there until an admin
# removes the marker.
#
# Trust model: every member of the group that owns the marker directory can
# mark ANY host, and so take it out of service. Put only the users who run
# hot-spare jobs in that group; use drain-qmod-helper.sh instead when a user
# may drain only the hosts of their own job.
#
# Setup (once, as root and a manager):
#   1. A shared marker directory every exec host can read, writable by the
#      group of hot-spare job users only (sticky: a member cannot remove
#      another member's marker), e.g.
#        groupadd slurm-shim; usermod -aG slurm-shim <user> ...
#        d=$SGE_ROOT/$SGE_CELL/common/slurm-shim/drain
#        mkdir -p "$d"; chgrp slurm-shim "$d"; chmod 1771 "$d"
#   2. The complex:   node_drained  nd  INT  >=  YES  NO  0  0
#        qconf -mc   (add the line above)
#   3. This script as the load sensor on every exec host:
#        qconf -mconf <host>   ->   load_sensor  /opt/slurm-shim/share/drain-load-sensor.sh
#   4. The threshold on the queues that hot-spare jobs use:
#        qconf -mattr queue load_thresholds node_drained=1 all.q
#   5. In the shim config:
#        drain_command: [touch, "/opt/ocs/default/common/slurm-shim/drain/{host}"]
# Undrain a host:  rm <drain dir>/<host>
#
# Only a regular file is a marker (a directory or symlink of that name is
# ignored). The sensor looks up its own short name (hostname -s), while
# {host} is the name the job's allocation uses: if exec hosts are known by
# their FQDN, use "hostname -f" below.
#
# The sensor must answer on every host in the queue: a missing load value puts
# the queue instance in alarm too.
DRAIN_DIR=${DRAIN_DIR:-${SGE_ROOT:-/opt/ocs}/${SGE_CELL:-default}/common/slurm-shim/drain}
host=$(hostname -s)
while read -r line; do
  [ "$line" = quit ] && exit 0
  drained=0
  [ -f "$DRAIN_DIR/$host" ] && [ ! -L "$DRAIN_DIR/$host" ] && drained=1
  echo begin
  echo "$host:node_drained:$drained"
  echo end
done
