#!/bin/sh
#___INFO__MARK_BEGIN_NEW__
###########################################################################
#
#  Copyright 2026 HPC-Gridware GmbH
#
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an "AS IS" BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.
#
###########################################################################
#___INFO__MARK_END_NEW__

# Reference PE start_proc_args for slurm-shim PE mode.
#
# Configure as the PE's start_proc_args so it runs on the master host before the
# job script. It runs the fabricator, which writes layout.json and the
# environment file (or the environment.failed sentinel on failure) into the
# per-job TMPDIR. The fabricator exits 0 even on failure so a non-zero
# start_proc_args cannot put the queue instance into an error state;
# the sourcing hook (run by the queue starter_method, or sourced by the job
# script) enforces the failure by checking the sentinel.
#
# Install slurm-shim on an identical absolute path on every host.

exec /opt/slurm-shim/bin/slurm-shim-env
