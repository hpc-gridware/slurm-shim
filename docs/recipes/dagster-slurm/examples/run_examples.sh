#!/bin/bash
# Run every dagster-slurm example against the cluster and check the outcome.
# Run ../run.sh once first: it creates the Python environment used here.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
export DAGSTER_SLURM_ENV="${DAGSTER_SLURM_ENV:-$HOME/dagster-slurm-env}"
[ -x "$DAGSTER_SLURM_ENV/env/bin/python" ] || { echo "run ../run.sh first" >&2; exit 1; }

# Where Dagster keeps its run history. `dagster dev` reads the same directory,
# so these runs show up in the UI afterwards.
export DAGSTER_HOME="${DAGSTER_HOME:-$HOME/dagster-home}"
mkdir -p "$DAGSTER_HOME"

export PATH="$DAGSTER_SLURM_ENV/env/bin:$PATH"
cd "$here"
exec python run_examples.py
