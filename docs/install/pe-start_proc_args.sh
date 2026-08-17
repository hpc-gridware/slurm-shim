#!/bin/sh
# Reference PE start_proc_args for slurm-shim PE mode.
#
# Configure as the PE's start_proc_args so it runs on the master host before the
# job script. It runs the fabricator, which writes layout.json and the
# environment file (or the environment.failed sentinel on failure) into the
# per-job TMPDIR. The fabricator exits 0 even on failure so a non-zero
# start_proc_args cannot put the queue instance into an error state;
# the sourcing hook enforces the failure by checking the sentinel.
#
# Install slurm-shim on an identical absolute path on every host.

exec /opt/slurm-shim/bin/slurm-shim-env
