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
LINKS := srun sbatch sacct scancel scontrol squeue sinfo slurm-shim-env slurm-shim-stepper

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

# --- Local OCS test cluster (reuses the quickinstall repo, unmodified) --------
# Stand up a real 3-node Open Cluster Scheduler cluster with slurm-shim installed
# and try it out. Needs Docker + Compose v2.
#   make cluster-up [OCS_VERSION=9.1.6] [ARGS=--gpu]   cluster + shim (default OCS: 9.1.6)
#   make demo / demo-gpu / demo-flax                   submit a demo job, print output
#   make demo-envcheck                                 diagnose the fabricated SLURM_* env
#   make cluster-down [ARGS=-v]                        tear down (-v also wipes OCS install)
CLUSTER := test/cluster
.PHONY: cluster-up cluster-down cluster-install demo demo-gpu demo-flax demo-envcheck
cluster-up:
	$(CLUSTER)/up.sh $(ARGS)
cluster-down:
	$(CLUSTER)/down.sh $(ARGS)
cluster-install:
	$(CLUSTER)/install-shim.sh $(ARGS)
demo:
	$(CLUSTER)/demo.sh cpu
demo-gpu:
	$(CLUSTER)/demo.sh gpu
demo-flax:
	$(CLUSTER)/demo.sh flax
demo-envcheck:
	$(CLUSTER)/demo.sh envcheck

# --- End-to-end + OCS version-compatibility suite -----------------------------
#   make e2e                          run all e2e checks against a running cluster
#   make capture-fixtures             refresh fixtures for the running OCS version
#   make e2e-matrix                   for each OCS_VERSION: down -v; up; e2e; capture
E2E     := test/e2e
E2E_OCS ?= 9.0.10 9.1.4 9.1.5 9.1.6
.PHONY: e2e capture-fixtures e2e-matrix
e2e:
	$(E2E)/run.sh
capture-fixtures:
	$(E2E)/capture.sh
e2e-matrix:
	@for v in $(E2E_OCS); do \
	  echo "===== OCS $$v ====="; \
	  OCS_VERSION=$$v $(CLUSTER)/down.sh -v || true; \
	  OCS_VERSION=$$v $(CLUSTER)/up.sh || exit 1; \
	  OCS_VERSION=$$v $(E2E)/run.sh || exit 1; \
	  OCS_VERSION=$$v $(E2E)/capture.sh || exit 1; \
	done

# ---- packaging ---------------------------------------------------------------
# The payload is the static binary, the queue starter, the hook, and the command
# links. The tarball IS the payload -- the same delivery model as OCS itself,
# which unpacks into $SGE_ROOT. `slurm-shim install` places it at the site.
# No rpm/deb: see docs/install/README.md ("Why no rpm or deb yet").
PAYLOAD  := dist/payload
ARCH     ?= $(shell go env GOARCH)

.PHONY: payload tarball checksums
# GOOS is pinned: the payload runs on cluster nodes, never on the build host, and
# ARCH must name what the binary actually is. Building from the host `build`
# target once produced a macOS binary inside a file called ...linux_arm64.tar.gz.
payload:
	rm -rf $(PAYLOAD)
	install -d $(PAYLOAD)/bin $(PAYLOAD)/etc $(PAYLOAD)/share
	GOOS=linux GOARCH=$(ARCH) CGO_ENABLED=0 go build -tags osusergo,netgo -trimpath \
	  -ldflags "$(LDFLAGS)" -o $(PAYLOAD)/bin/slurm-shim ./cmd/slurm-shim
	install -m 0755 docs/install/slurm-shim-starter.sh $(PAYLOAD)/bin/slurm-shim-starter
	install -m 0644 docs/install/slurm-shim-source-hook.sh $(PAYLOAD)/etc/slurm-shim-source-hook.sh
	install -m 0755 docs/install/drain-load-sensor.sh docs/install/drain-qmod-helper.sh $(PAYLOAD)/share/
	@for l in $(LINKS); do ln -sf slurm-shim $(PAYLOAD)/bin/$$l; done
	install -m 0755 scripts/install.sh dist/install.sh

tarball: payload
	tar -C $(PAYLOAD) -czf dist/slurm-shim_linux_$(ARCH).tar.gz bin etc share

checksums:
	cd dist && (command -v sha256sum >/dev/null && sha256sum *.tar.gz 2>/dev/null || shasum -a 256 *.tar.gz 2>/dev/null) > SHA256SUMS && cat SHA256SUMS

# ---- third-party licenses and SBOM ---------------------------------------------
# 3rdparty-licenses/ in the layout qontrol uses: a CycloneDX SBOM from Trivy
# (sbom.cyclonedx.json), the license text of every third-party Go module linked
# into the binary (licenses/, by go-licenses), and a module,URL,license index
# (licenses.csv). Test-only modules (ginkgo, gomega) are not linked into the
# binary and so not listed by go-licenses; the SBOM covers all of go.mod.
# hpc-gridware modules are our own and are skipped, as in qontrol.
#
# Both tools run in containers, so the result does not depend on what the host
# has installed; the release workflow runs this target for every release.
# --user keeps the output owned by the caller, not root. Errors fail the target.
.PHONY: licenses-sbom
licenses-sbom:
	rm -rf 3rdparty-licenses
	mkdir -p 3rdparty-licenses
	docker run --rm --user $$(id -u):$$(id -g) -v $(CURDIR):/workspace -w /workspace \
		aquasec/trivy:latest fs --cache-dir /tmp/trivy --format cyclonedx \
		--skip-dirs dist,bin,test/cluster/.quickinstall \
		--output 3rdparty-licenses/sbom.cyclonedx.json .
	docker run --rm --user $$(id -u):$$(id -g) -v $(CURDIR):/workspace -w /workspace \
		-e HOME=/tmp -e GOPATH=/tmp/go -e GOCACHE=/tmp/gocache -e GOFLAGS=-buildvcs=false \
		-e GOPROXY=https://proxy.golang.org,direct -e GOTOOLCHAIN=auto -e GOWORK=off \
		-e GOOS=linux -e CGO_ENABLED=0 \
		golang:1.25 sh -c '\
		go install github.com/google/go-licenses@latest && \
		/tmp/go/bin/go-licenses save ./... --save_path=3rdparty-licenses/licenses --ignore github.com/hpc-gridware --force && \
		/tmp/go/bin/go-licenses csv  ./... --ignore github.com/hpc-gridware > 3rdparty-licenses/licenses.csv'
	@echo "3rd party licenses and SBOM generated in 3rdparty-licenses/"

# ---- releases/ ---------------------------------------------------------------
# releases/ holds the latest release: the tarballs and SHA256SUMS exactly as
# published on GitHub, plus a README for installing from them. The release
# workflow fills it from the very files it uploads, so they are byte-identical.
# To fill it by hand from a published release instead:
#   gh release download vX.Y.Z -D dist --clobber -p '*.tar.gz' -p SHA256SUMS
#   make releases-dir RELEASE=vX.Y.Z
RELEASE ?= $(VERSION)
.PHONY: releases-dir
releases-dir:
	test -f dist/SHA256SUMS
	mkdir -p releases
	rm -f releases/*.tar.gz releases/SHA256SUMS
	cp dist/slurm-shim_linux_amd64.tar.gz dist/slurm-shim_linux_arm64.tar.gz dist/SHA256SUMS releases/
	cd releases && (command -v sha256sum >/dev/null && sha256sum -c SHA256SUMS || shasum -a 256 -c SHA256SUMS)
	sed 's/@VERSION@/$(RELEASE)/g' docs/install/releases-README.md.in > releases/README.md
