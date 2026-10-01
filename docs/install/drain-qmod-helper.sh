#!/bin/sh
# Reference privileged drain helper for hot-spare swaps (sbatch --x-spares):
# disables every queue instance on the host a swap took out of a job, and logs
# who asked and why. Run through sudo, so the job user needs no Grid Engine
# rights and every drain is auditable.
#
# Trust model: the caller can drain only a host granted to one of its own
# jobs that is running now. sudo sets SUDO_USER, which is the only trusted
# input; host and job are checked against "qstat -j <job> -xml" (job owner ==
# SUDO_USER, host in the job's granted host list) before qmod runs. Anything
# else is refused with exit 2.
#
# Setup:
#   /etc/sudoers.d/slurm-shim-drain:
#     ALL ALL=(sgeadmin) NOPASSWD: /opt/slurm-shim/share/drain-qmod-helper.sh
#   shim config:
#     drain_command: [sudo, -n, -u, sgeadmin, /opt/slurm-shim/share/drain-qmod-helper.sh, "{host}", "{job}", "{reason}"]
#   sgeadmin must be a Grid Engine operator or manager.
#
# {host} is the host name as the job's allocation names it (PE_HOSTFILE), and
# the helper disables "*@{host}". If the queue instances of a host use another
# form of its name (short vs. FQDN), adapt the qmod pattern below.
#
# Works under the default sudoers env_reset and secure_path: the Grid Engine
# environment comes from settings.sh below, never from the caller.
# Undrain a host:  qmod -e '*@<host>'

# Site settings. Deliberately not read from the environment: a caller-chosen
# SGE_ROOT would make sudo source the caller's settings.sh.
SGE_ROOT=/opt/ocs
SGE_CELL=default

PATH=/usr/bin:/bin
export PATH SGE_ROOT SGE_CELL
. "$SGE_ROOT/$SGE_CELL/common/settings.sh"
set -eu

refuse() {
  echo "drain-qmod-helper: refusing: $*" >&2
  exit 2
}

host=${1:-} job=${2:-} reason=${3:-slurm-shim drain}
caller=${SUDO_USER:-}
case $host in
  ''|-*|*[!A-Za-z0-9._-]*) refuse "host name '$host'" ;;
esac
case $job in
  ''|*[!0-9]*) refuse "job id '$job'" ;;
esac
[ -n "$caller" ] || refuse "no SUDO_USER (run me through sudo)"
# The reason is caller text: keep it to a short, printable line for the log.
reason=$(printf '%s' "$reason" | tr -cd 'A-Za-z0-9 ._:,/=-' | cut -c1-200)

xml=$(qstat -j "$job" -xml 2>/dev/null) || refuse "qstat -j $job failed"
owner=$(printf '%s\n' "$xml" | sed -n 's:.*<JB_owner>\(.*\)</JB_owner>.*:\1:p' | head -n 1)
[ -n "$owner" ] || refuse "job $job does not exist"
[ "$owner" = "$caller" ] || refuse "job $job is not a job of $caller"
printf '%s\n' "$xml" | grep -qF "<JG_qhostname>$host</JG_qhostname>" ||
  refuse "host $host is not granted to running job $job"

logger -t slurm-shim-drain "user $caller job $job drains $host: $reason" || true
exec qmod -d "*@$host"
