#!/bin/bash
#SBATCH --job-name=clearml-{{task_id}}
#SBATCH --partition={{queue}}
#SBATCH --nodes={{num_nodes}}
#SBATCH --ntasks-per-node=1
#SBATCH --cpus-per-task={{cpus}}
#SBATCH --output={{log_dir}}/clearml-{{task_id}}-%j.out
#
# Site sbatch template for clearml-agent in SLURM mode, targeting the slurm-shim.
# clearml-agent renders {{...}} placeholders per enqueued task and submits this
# with `sbatch`; the shim translates it to `qsub -terse` and echoes exactly
# "Submitted batch job <id>", which the agent parses. Unknown/container
# directives the agent may add are warn-and-ignored by the shim, not errors.
#
# NOTE: placeholder names below must match YOUR clearml-agent version's template
# variables (clearml-agent SLURM mode is Enterprise + closed-source). Treat this
# as a starting point and reconcile against a real rendered template.
set -euo pipefail

# Provision the task's environment however your site does (module load / conda /
# venv / container). Example:
#   module load cuda/12.4
#   source /opt/venvs/{{task_id}}/bin/activate

# Hand control to the agent to execute the specific task in place.
exec clearml-agent execute --id "{{task_id}}" --standalone-mode true
