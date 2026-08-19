#!/bin/bash
# Bootstrap a shared venv with submitit and run the smoke test against the shim.
# Run this on the submit host (where the slurm-shim sbatch/srun/sacct/scancel
# symlinks are on PATH). The venv and the run dir live on the shared filesystem
# so every compute node sees the same submitit and the same result pickles.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
venv="${VENV:-$HOME/submitit-venv}"
workdir="${WORKDIR:-$HOME/submitit-run}"

if [ ! -x "$venv/bin/python" ]; then
	python3 -m venv "$venv"
	"$venv/bin/pip" install --quiet --upgrade pip
	"$venv/bin/pip" install --quiet submitit
fi

mkdir -p "$workdir"
cd "$workdir"
exec "$venv/bin/python" "$here/submitit_smoke.py"
