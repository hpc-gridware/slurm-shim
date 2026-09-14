# Security Policy

HPC-Gridware takes the security of its products and open-source projects seriously
and appreciates responsible disclosure.

## Reporting a vulnerability

Please do **not** open a public GitHub issue. Report it through one of:

- Web form: https://www.hpc-gridware.com/security
- Email: sales@hpc-gridware.com

Include what is needed to assess it:

- a description of the vulnerability and its impact
- the slurm-shim version (`sbatch --version`) and the scheduler version
  (`qconf -help | head -1`)
- steps to reproduce, or a proof of concept

Reports are handled by the HPC-Gridware product security team in line with its
vulnerability handling process, including the requirements of the EU Cyber
Resilience Act.

## Coordinated disclosure

Please keep the issue private until it has been assessed and a fix is available, or
until disclosure has been agreed.

## Supported versions

Security fixes are made in the latest release of slurm-shim.

## Scope

Of particular interest are issues that let one user affect another user's jobs or
data on a shared cluster, for example through the per-step launch token, job
environment fabrication, the queue `starter_method`, or files the shim writes on
execution hosts.
