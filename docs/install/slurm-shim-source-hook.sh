#!/bin/sh
# Reference sourcing hook for slurm-shim PE mode.
#
# Source this from the site job wrapper or the first line of the job script:
#   . /opt/slurm-shim/etc/slurm-shim-source-hook.sh
#
# It reconciles the three fabricator outcomes and performs the ownership and
# permission checks that guard against a co-tenant pre-planting a file in the
# predictable per-job TMPDIR. A production deployment MAY instead delegate the
# checks to the shim binary once M2 ships a validating subcommand; this shell
# reference keeps the contract explicit and dependency-free.
#
# Aborting: the abort paths call `exit 1`, which -- because this file is SOURCED
# -- ends the job script itself. That is deliberate: enforcement belongs here,
# not in every call site. A bare `. hook.sh` must be safe, because that is what
# the README, the recipes and every hand-written job script actually do; a hook
# that only RETURNED non-zero would let a job whose environment failed to
# fabricate run on degraded (a 4-node training silently on 1 node) unless the
# caller remembered `|| exit 1`. Signalling ($$ + SIGTERM) was considered and
# rejected: an ordinary `trap ... TERM` cleanup handler in the job script
# swallows it, and GE would record the self-kill as exit 143, which sacct
# reports as ExitCode 0:15 -- indistinguishable from an operator qdel.
#
# The "continue" paths below MUST stay `return 0`, never `exit 0`: exiting zero
# here would end the job successfully on the spot, which is a far worse failure
# than the one this file guards against.

slurm_shim_state="${TMPDIR:-/tmp}/slurm_shim"
slurm_shim_env="$slurm_shim_state/environment"
slurm_shim_sentinel="$slurm_shim_state/environment.failed"
slurm_shim_missing="${SLURM_SHIM_HOOK_MISSING_ENV:-continue}"

# (a) Sentinel present: fabrication failed; abort the job so it does not run
# silently degraded (e.g. a multi-node training on one node).
if [ -e "$slurm_shim_sentinel" ]; then
	echo "slurm-shim: fabrication failed (sentinel present); aborting job" >&2
	exit 1
fi

# (c) Neither file present: follow the configured policy. Default "continue" so
# a site-wide hook does not kill jobs that never ran the fabricator.
if [ ! -e "$slurm_shim_env" ]; then
	if [ "$slurm_shim_missing" = "abort" ]; then
		echo "slurm-shim: no environment file; aborting job" >&2
		exit 1
	fi
	return 0 2>/dev/null || exit 0
fi

# (b) Environment file present: validate before sourcing.
# Reject a symlink; require a regular file owned by the current user and not
# group/world writable. Any failure is treated as "missing".
slurm_shim_reject() {
	echo "slurm-shim: environment file failed the safety check ($1); treating as missing" >&2
	if [ "$slurm_shim_missing" = "abort" ]; then
		exit 1
	fi
	return 0 2>/dev/null || exit 0
}

if [ -h "$slurm_shim_env" ]; then
	slurm_shim_reject "is a symlink"
elif [ ! -f "$slurm_shim_env" ]; then
	slurm_shim_reject "not a regular file"
elif [ -z "$(find "$slurm_shim_env" -maxdepth 0 -user "$(id -un)" 2>/dev/null)" ]; then
	slurm_shim_reject "not owned by the current user"
elif [ -n "$(find "$slurm_shim_env" -maxdepth 0 \( -perm -0020 -o -perm -0002 \) 2>/dev/null)" ]; then
	slurm_shim_reject "is group- or world-writable"
else
	. "$slurm_shim_env"
fi
