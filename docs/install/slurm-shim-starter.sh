#!/bin/sh
# Reference Open Cluster Scheduler (fka Sun Grid Engine) starter_method for
# slurm-shim (REQ-FAB-010).
#
# Set it once per queue and every job in that queue gets the fabricated SLURM_*
# environment with no line added to the job script -- an unmodified SLURM batch
# script sees what it would see on a real SLURM cluster:
#
#   qconf -mattr queue starter_method /opt/slurm-shim/bin/slurm-shim-starter all.q
#
# Open Cluster Scheduler invokes this with the job script (and its arguments) in
# "$@". It sources the hook, which reconciles the fabricator's outcome (source
# the environment / abort on a failed fabrication / continue when no fabrication
# ran), then starts the job the way the scheduler itself would have. A non-zero
# exit here fails the JOB with exit 1; it does not error the queue instance the
# way a failing start_proc_args would (verified live on OCS 9.1.5: sacct reports
# FAILED, ExitCode 1:0, and every queue instance stays schedulable).
#
# Native jobs: Open Cluster Scheduler tells a starter_method what it would have
# done itself, in three variables (sge_queue_conf(5), starter_method):
#   SGE_STARTER_SHELL_START_MODE   the queue's shell_start_mode
#   SGE_STARTER_SHELL_PATH         the shell it would have used (-S, else the
#                                  queue's shell); empty on the qrsh path
#   SGE_STARTER_USE_LOGIN_SHELL    "true" when that shell is in login_shells
# This file honours them, so a job in the queue that never asked for the shim
# starts exactly as it would have without the starter: under unix_behavior and
# raw_exec the script runs itself (its shebang decides); under posix_compliant
# and script_from_stdin the requested shell runs it, as a login shell when the
# scheduler would have made it one. A login shell is what the scheduler makes it:
# argv[0] = "-<shell>". A POSIX shell script cannot set argv[0], so that is done
# through bash's `exec -a` when bash is present (which is the normal case on an
# exec host); without bash, shells that accept -l get it, and csh/tcsh -- which
# reject -l when a script follows -- run as non-login shells rather than fail.
#
# Security: this runs as the job user, for every job in the queue. It and the
# hook MUST be root-owned and not group/world-writable, or whoever can edit them
# runs code inside every other user's jobs.
#
# Install slurm-shim on an identical absolute path on every host. The hook is
# located relative to THIS script (../etc/slurm-shim-source-hook.sh), so a site
# installing under its own prefix cannot end up with a starter that reads the
# hook from a different one.

# A starter with nothing to start must never report success for work that did
# not happen: `exec` with no arguments is a no-op that returns 0.
[ "$#" -gt 0 ] || { echo "slurm-shim-starter: no command to start" >&2; exit 1; }

# Shim-internal launches: srun starts its per-host stepper over qrsh -inherit,
# which passes through this starter too. The stepper carries its environment
# over srun's control channel and must never be gated on the hook -- on a slave
# host the fabricator never ran, and a site policy of
# SLURM_SHIM_HOOK_MISSING_ENV=abort would otherwise kill every multi-node step.
#
# Match argv[1] and argv[2] SEPARATELY. Matching a flattened "$1 $2" cannot tell
# the argv separator from a space inside an argument, so a job argument
# containing "/slurm-shim-stepper " was enough to skip the hook and run the job
# with no SLURM_* at all. The launcher's argv is built by buildQrshArgs in
# internal/launch/qrsh.go, and internal/launch/starter_contract_test.go feeds
# this file that function's real output so the two cannot drift apart.
case "$1" in
*/slurm-shim) [ "$2" = stepper ] && exec "$@" ;;
esac

# Locate the hook next to this script rather than at a fixed path. Grid Engine
# invokes a starter_method by absolute path, so $0 carries the install prefix
# (verified on the batch, qrsh-command and stepper paths); the bare-name branch
# is insurance for a flavour that passes only the basename.
case "$0" in
*/*) slurm_shim_hook="${0%/*}/../etc/slurm-shim-source-hook.sh" ;;
*)   slurm_shim_hook="/opt/slurm-shim/etc/slurm-shim-source-hook.sh" ;;
esac

# A missing hook is a broken install, not a host without the shim: Grid Engine
# had to exec THIS script out of the same tree to get here. Saying nothing would
# run every job in the queue with no SLURM_* environment and no diagnostic --
# the silent degradation the hook exists to prevent (a 4-node training on 1
# node). So report it, and honour the same policy knob the hook uses for its own
# "nothing was fabricated" case.
if [ -r "$slurm_shim_hook" ]; then
	. "$slurm_shim_hook"
else
	echo "slurm-shim-starter: cannot read $slurm_shim_hook; this job gets no SLURM_* environment" >&2
	if [ "${SLURM_SHIM_HOOK_MISSING_ENV:-continue}" = abort ]; then
		echo "slurm-shim-starter: aborting job (SLURM_SHIM_HOOK_MISSING_ENV=abort)" >&2
		exit 1
	fi
fi

# slurm_shim_start_shell <shell> <args...>: exec the shell the scheduler would
# have used, as a login shell when it would have (see the header for how).
slurm_shim_start_shell() {
	slurm_shim_shell="$1"; shift
	if [ "${SGE_STARTER_USE_LOGIN_SHELL:-}" = true ]; then
		if command -v bash >/dev/null 2>&1; then
			exec bash -c 'exec -a "$0" "$@"' "-${slurm_shim_shell##*/}" "$slurm_shim_shell" "$@"
		fi
		case "${slurm_shim_shell##*/}" in
		csh|tcsh) ;;
		*) exec "$slurm_shim_shell" -l "$@" ;;
		esac
	fi
	exec "$slurm_shim_shell" "$@"
}

# Start the job the way the scheduler would have (see the header).
case "${SGE_STARTER_SHELL_START_MODE:-unix_behavior}" in
posix_compliant|script_from_stdin)
	# "$shell script args", or "$shell -s args" with the script on stdin -- the
	# scheduler has already put -s in $1 for that mode. An empty shell path is
	# the qrsh path, which runs the command itself.
	[ -n "${SGE_STARTER_SHELL_PATH:-}" ] && slurm_shim_start_shell "$SGE_STARTER_SHELL_PATH" "$@"
	;;
start_as_command)
	[ -n "${SGE_STARTER_SHELL_PATH:-}" ] && slurm_shim_start_shell "$SGE_STARTER_SHELL_PATH" -c "$1"
	;;
esac
exec "$@"
