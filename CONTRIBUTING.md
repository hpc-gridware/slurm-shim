# Contributing to slurm-shim

Bug reports, compatibility reports and pull requests are all welcome.

## Reporting

- **A SLURM script or launcher that does not work** — open a [bug report](../../issues/new/choose).
  The failing script, `sbatch --version`, and `slurm-shim doctor` output make it
  reproducible.
- **A flag, variable or tool that is not supported yet** — open a feature request,
  naming the tool that needs it.
- **A security issue** — do not open a public issue; see [SECURITY.md](SECURITY.md).

## Development

Requires Go 1.25 and `golangci-lint`.

```bash
make build       # static binary in ./bin
make test        # unit tests (Ginkgo)
make lint        # golangci-lint
make fmt         # gofmt -s + goimports
```

End-to-end tests run against a real Open Cluster Scheduler cluster in containers
(Docker 20.10+ with Compose v2, about 4 GB RAM):

```bash
make cluster-up  # boot a 3-node OCS cluster and install the shim
make e2e         # run test/e2e against it
make cluster-down
```

## Where things live

| To change | Look in |
|---|---|
| an `sbatch` / `#SBATCH` flag | `internal/cli/sbatch/translate.go` |
| the `SLURM_*` environment a job sees | `internal/fabricator` |
| `srun` launch, per-rank env, GPU masks | `internal/cli/srun`, `internal/launch` |
| a command's output (`squeue`, `sacct`, ...) | `internal/cli/<command>` |
| installer and `doctor` | `internal/cli/installcmd`, `internal/cli/doctor` |

Parsing of Grid Engine command output belongs in
[go-clusterscheduler](https://github.com/hpc-gridware/go-clusterscheduler). If a
parser is missing or wrong there, fix it upstream rather than adding one here.

## Pull requests

- Keep them focused: one behaviour change per PR.
- Tests are part of the change. Tests use Ginkgo and live next to the code they
  cover; a bug fix includes the test that would have caught it.
- If behaviour a user can see changes, update the
  [compatibility matrix](README.md#compatibility-matrix) in the same PR.
- `make test`, `make lint` and `make tidy-check` must pass; CI runs the same.
- Commit messages say *why*, not only what.

## License

slurm-shim is licensed under [Apache-2.0](LICENSE). Contributions are accepted
under the same license, as described in section 5 of the license.
