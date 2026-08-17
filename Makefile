# slurm-shim build automation.
#
# The distributable artifact is a single static binary (CGO disabled, pure-Go
# user/DNS resolvers) with symlink dispatch; see docs/plans for the design.

BINDIR      := bin
BIN         := $(BINDIR)/slurm-shim
PKG         := github.com/hpc-gridware/slurm-shim
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS     := -X $(PKG)/internal/version.Shim=$(VERSION)

# Symlink farm installed alongside the binary (spec section 3.1). Each name
# dispatches back to the real binary via filepath.Base(os.Args[0]).
LINKS := srun sbatch scancel scontrol squeue sinfo slurm-shim-env slurm-shim-stepper

# Static, reproducible build. osusergo/netgo force the pure-Go user and DNS
# resolvers so an accidental cgo-enabled build cannot change behavior; the real
# NSS/LDAP fallback is handled in-process via getent (see the Identity seam).
.PHONY: build
build:
	CGO_ENABLED=0 go build -tags osusergo,netgo -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/slurm-shim

# Install the busybox-style symlink farm next to the binary.
.PHONY: install-links
install-links: build
	@for l in $(LINKS); do ln -sf slurm-shim $(BINDIR)/$$l; done
	@echo "installed links: $(LINKS)"

.PHONY: test
test:
	go test ./...

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: fmt
fmt:
	gofmt -s -w .
	goimports -w .

# Requirement-traceability gate (REQ-TST-001, SI-47). Pass MILESTONE=Mn to
# scope the diff to one milestone; bare `make trace` reports the full matrix
# advisory (M8 adds -strict as the release gate). SPEC points at the current
# authoritative spec so retired ids in older drafts are not counted.
SPEC ?= docs/specs/2026-08-11-slurm-shim-spec-v1.1.md
.PHONY: trace
trace:
	go run ./tools/trace -spec $(SPEC) $(if $(MILESTONE),-milestone $(MILESTONE),)

# Fail if go.mod/go.sum are not tidy, so CI catches drift.
.PHONY: tidy-check
tidy-check:
	go mod tidy
	git diff --exit-code go.mod go.sum

.PHONY: vet
vet:
	go vet ./...

.PHONY: clean
clean:
	rm -rf $(BINDIR)
