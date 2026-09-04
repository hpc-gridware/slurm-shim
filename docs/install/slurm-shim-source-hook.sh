#!/bin/sh
# Reference sourcing hook for slurm-shim PE mode.
#
# The queue's starter_method (slurm-shim-starter.sh, alongside this file) sources
# it for every job, so an unmodified SLURM batch script needs no line at all. On
# a site that has not wired a starter, source it from the site job wrapper or
# the first line of the job script:
#   . /opt/slurm-shim/etc/slurm-shim-source-hook.sh
#
# It reconciles the three fabricator outcomes -- environment written, fabrication
# failed (sentinel), nothing fabricated -- and it is the trust boundary for the
# per-job state directory (REQ-FAB-009, REQ-FAB-010).
#
# That directory lives under the job's TMPDIR, which sits at a PREDICTABLE path
# (/tmp/<job>.<task>.<queue>) in a world-writable /tmp, and Open Cluster
# Scheduler tolerates a pre-existing directory there: it chowns it to the job
# owner without resetting the mode. A co-tenant can therefore hand a job a TMPDIR
# they can still write to and plant a failure sentinel (which would kill the job)
# or an environment file (which would run as the job owner) inside it. So before
# this file believes anything in that directory, TMPDIR and the state directory
# must both be real directories owned by the current user that nobody else can
# write, and the sentinel and environment files must be regular, owned files that
# nobody else can write. Whatever fails those checks is reported on stderr and
# treated as "nothing fabricated". Because no other user can then rename entries
# in those directories, the file that was checked is the file that gets sourced.
# There is no /tmp fallback: an unset TMPDIR means no per-job state.
#
# These checks defend the job against a co-tenant. They cannot defend it against
# its own owner, who controls PATH and therefore `find` and `id`.
#
# Aborting: the abort paths call `exit 1`, which -- because this file is SOURCED
# -- ends the job script itself. That is deliberate: enforcement belongs here,
# not in every call site. A bare `. hook.sh` must be safe, because that is what
# the starter, the recipes and every hand-written job script actually do; a hook
# that only RETURNED non-zero would let a job whose environment failed to
# fabricate run on degraded (a 4-node training silently on 1 node) unless the
# caller remembered `|| exit 1`. Signalling ($$ + SIGTERM) was considered and
# rejected: an ordinary `trap ... TERM` cleanup handler in the job script
# swallows it, and GE would record the self-kill as exit 143, which sacct
# reports as ExitCode 0:15 -- indistinguishable from an operator qdel.
#
# The "continue" path at the end MUST stay `return 0`, never `exit 0`: exiting
# zero there would end the job successfully on the spot, which is a far worse
# failure than the one this file guards against. Every decision is made at the
# top level of this file for the same reason: `return` inside a function only
# leaves the function.

slurm_shim_missing="${SLURM_SHIM_HOOK_MISSING_ENV:-continue}"
slurm_shim_state="${TMPDIR:-}/slurm_shim"
slurm_shim_env="$slurm_shim_state/environment"
slurm_shim_sentinel="$slurm_shim_state/environment.failed"

slurm_shim_warn() { echo "slurm-shim: $1" >&2; }

# slurm_shim_private -d|-f <path>: the path is of that type, is not a symlink,
# is owned by the current user, and is not group- or world-writable. Symlinks
# are rejected because -d/-f follow them.
slurm_shim_private() {
	case "$1" in
	-d) [ -d "$2" ] || return 1 ;;
	-f) [ -f "$2" ] || return 1 ;;
	*) return 1 ;;
	esac
	[ ! -h "$2" ] || return 1
	[ -n "$(find "$2" -maxdepth 0 -user "$(id -u)" ! -perm -0020 ! -perm -0002 2>/dev/null)" ]
}

# Is there a state directory we may trust at all?
slurm_shim_trusted='no'
if [ -z "${TMPDIR:-}" ]; then
	slurm_shim_warn "TMPDIR is not set; ignoring per-job state"
elif ! slurm_shim_private -d "$TMPDIR"; then
	slurm_shim_warn "TMPDIR $TMPDIR is not a private directory of this job; ignoring per-job state"
elif [ ! -e "$slurm_shim_state" ] && [ ! -h "$slurm_shim_state" ]; then
	: # nothing fabricated (a native job, or a slave host): silent
elif ! slurm_shim_private -d "$slurm_shim_state"; then
	slurm_shim_warn "$slurm_shim_state is not a private directory of this job; ignoring it"
else
	slurm_shim_trusted='yes'
fi

# Which of the fabricator's outcomes is present, and is the file itself ours?
slurm_shim_outcome='none'
if [ "$slurm_shim_trusted" = yes ]; then
	if [ -e "$slurm_shim_sentinel" ] || [ -h "$slurm_shim_sentinel" ]; then
		if slurm_shim_private -f "$slurm_shim_sentinel"; then
			slurm_shim_outcome='sentinel'
		else
			slurm_shim_warn "$slurm_shim_sentinel failed the safety check; ignoring it"
		fi
	fi
	if [ "$slurm_shim_outcome" = none ] && { [ -e "$slurm_shim_env" ] || [ -h "$slurm_shim_env" ]; }; then
		if slurm_shim_private -f "$slurm_shim_env"; then
			slurm_shim_outcome='env'
		else
			slurm_shim_warn "environment file failed the safety check; treating as missing"
		fi
	fi
fi

case "$slurm_shim_outcome" in
sentinel)
	# (a) Fabrication failed: abort so the job does not run silently degraded
	# (e.g. a multi-node training on one node).
	slurm_shim_warn "fabrication failed (sentinel present); aborting job"
	exit 1
	;;
env)
	# (b) Sourcing the validated file. Everything above guarantees nobody else
	# can swap it between the check and this line.
	. "$slurm_shim_env"
	;;
*)
	# (c) Nothing trustworthy was fabricated: follow the policy. Default
	# "continue" so a site-wide hook does not kill jobs that never ran the
	# fabricator; SLURM_SHIM_HOOK_MISSING_ENV=abort makes it fatal, per job only
	# -- set cluster-wide it would kill every native job in the queue.
	if [ "$slurm_shim_missing" = "abort" ]; then
		slurm_shim_warn "no environment file; aborting job"
		exit 1
	fi
	return 0 2>/dev/null || exit 0
	;;
esac
