# clearml-agent (SLURM mode) on slurm-shim

clearml-agent's SLURM mode is the shim's headline commercial target. The agent
renders a site-authored `#SBATCH` template per enqueued task, submits it with
`sbatch`, parses the returned job id, and polls `squeue` until the task finishes.
Every one of those touchpoints is implemented in the shim -- so the integration
is **wiring + a template**, not new code.

## What the shim gives clearml

| clearml needs | shim provides |
|---------------|---------------|
| Submit a rendered `#SBATCH` script | `sbatch` -> `qsub -terse`; unknown/`--container-*` directives warn-and-ignored (REQ-SBT-001) |
| Read back the job id | prints exactly `Submitted batch job <id>` (REQ-SBT-003) |
| Poll running/queued/finished state | `squeue` with GE->SLURM state mapping (REQ-SQU-001..003) |
| Abort a task | `scancel` -> `qdel` (REQ-SCL-001) |
| Partition -> resources | `partitions` config maps the `--partition` to a GE queue + PE + slot count |

## Set it up

1. Put the shim symlinks on `PATH` ahead of any real SLURM client so `sbatch`,
   `squeue`, `scancel`, etc. resolve to `slurm-shim`.
2. Configure a partition in the shim config that the template's `--partition`
   refers to, e.g.:
   ```yaml
   partitions:
     gpu:   {queue: gpu.q,  pe: gpu.pe, slots: "per-task"}
     batch: {queue: all.q,  pe: smp.pe, slots: "16"}
   ```
3. Point clearml-agent at the shim CLIs and your template
   ([`clearml-agent.conf.snippet`](clearml-agent.conf.snippet)).
4. Use [`clearml_slurm_template.sh`](clearml_slurm_template.sh) as the starting
   `#SBATCH` template; the agent fills its `{{...}}` placeholders per task.

## Honest status: validate against the real agent

clearml-agent SLURM mode is **Enterprise and closed-source**, so the exact
template-variable names, config keys, and squeue-field expectations cannot be
confirmed from outside. The shim implements the SLURM-side contract to spec; the
remaining work is a one-time reconciliation once a licensed agent is available.

Validation checklist (run against a licensed clearml-agent):

- [ ] Enqueue a task; confirm the agent's rendered script submits and the agent
      logs the id it parsed from `Submitted batch job <id>`.
- [ ] Confirm the template's placeholder names match what the agent substitutes
      (adjust `clearml_slurm_template.sh`).
- [ ] Watch the agent's `squeue` polling: verify it reads the columns/states it
      expects (PD/R/CG/CD...) without parse errors, through completion.
- [ ] Abort a task from the ClearML UI; confirm `scancel` -> `qdel` removes it.
- [ ] Confirm the task reaches `completed` and the agent log is parse-error free
      (this is acceptance criterion AC5).
