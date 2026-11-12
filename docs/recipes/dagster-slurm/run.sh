#!/bin/bash
# Bootstrap a shared Python environment with dagster-slurm and run the smoke
# test against the shim. Run this on the submit host (where the slurm-shim
# sbatch/squeue/sacct/scancel symlinks are on PATH) as a user who can
# `ssh <submit host>` with a key and no password.
#
# The environment lives on the shared filesystem in the layout dagster-slurm
# expects from a pre-deployed environment:
#   $DAGSTER_SLURM_ENV/env/          the Python environment
#   $DAGSTER_SLURM_ENV/activate.sh   sourced by every job before the payload
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
export DAGSTER_SLURM_ENV="${DAGSTER_SLURM_ENV:-$HOME/dagster-slurm-env}"
python="${PYTHON:-python3.11}"   # dagster-slurm needs Python 3.11 or newer

# activate.sh is written last, so its absence means the environment is not
# complete -- also after an install that was interrupted half way.
if [ ! -f "$DAGSTER_SLURM_ENV/activate.sh" ]; then
	mkdir -p "$DAGSTER_SLURM_ENV"
	"$python" -m venv "$DAGSTER_SLURM_ENV/env"
	"$DAGSTER_SLURM_ENV/env/bin/pip" install --quiet --upgrade pip
	# dagster-webserver is the UI (`dagster dev`); the jobs do not need it.
	# Validated with dagster-slurm 1.24.2 and dagster 1.13.29.
	"$DAGSTER_SLURM_ENV/env/bin/pip" install --quiet dagster-slurm dagster-webserver
	printf '. "%s/env/bin/activate"\n' "$DAGSTER_SLURM_ENV" > "$DAGSTER_SLURM_ENV/activate.sh"
fi

exec "$DAGSTER_SLURM_ENV/env/bin/python" "$here/pipeline.py"
