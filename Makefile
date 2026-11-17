# isopress make targets. `make help` lists them.
#
# The gate targets mirror the invocations README.md documents in
# "Development and testing" (test tiers by build tag, the static-check
# gates, and the two Go bin-directory tools), so `make <target>` and
# the README's copy-paste line run the same command.

# Development tools live in the Go bin directory, deliberately not on
# PATH by default (README: resolve $(go env GOPATH)/bin, then run the
# tool by path). Same resolution here, so the targets work unchanged
# in a fresh shell.
GOPATH_BIN := $(shell go env GOPATH)/bin

.PHONY: build test test-e2e test-perf test-race vet verify vulncheck lint help publish deb appimage

build:
	go build -o bin/isopress ./cmd/isopress

# test runs the default fast suite (the inner loop; perf and e2e are
# separate build tags so timing tripwires cannot destabilise it). The
# explicit -timeout matches RELEASING.md's gate line and gives the
# slowest package real headroom (internal/cli ran 465.9s of the
# default 600s per-package timeout on the internal runner - a busy
# day would have flaked it).
test:
	go test ./... -timeout 30m

# vet is the plain static-check pass (README documents it standalone
# and as part of `verify`).
vet:
	go vet ./...

# test-e2e adds the end-to-end tiers (real catalogs, xorriso round
# trips, crash/resume, interrupts).
test-e2e:
	go test -tags e2e ./... -timeout 30m

# test-perf runs ONLY the performance smokes (walk+hash of a generated
# 100k-file tree and the 1M-item plan, each under its own budget).
# -tags perf is additive, so without -run '^TestPerf' the tier would
# re-run the entire default suite too and blow the 10m budget on a
# slow runner; the default-suite coverage comes from `make test` one
# step earlier in the documented workflow (and in CI).
test-perf:
	go test -tags perf ./... -timeout 10m -run '^TestPerf'

# test-race is the full race gate; -count=1 defeats the test cache so
# the detector really runs every time. Not part of `verify` (minutes).
test-race:
	go test -tags e2e -race -timeout 30m -count=1 ./...

# verify is the fast static gate: build, the three vet passes (one per
# build tag), a formatting check, and the module-integrity check.
verify: build
	go vet ./...
	go vet -tags e2e ./...
	go vet -tags perf ./...
	@unformatted="$$(gofmt -l .)"; \
		if [ -n "$$unformatted" ]; then \
			echo "gofmt needed on:" >&2; echo "$$unformatted" >&2; exit 1; \
		fi
	go mod verify

# vulncheck and lint run the Go bin-directory tools README documents
# (staticcheck, golangci-lint, govulncheck); each fails with an
# install hint when the binary is missing instead of a bare
# "command not found".
vulncheck:
	@test -x "$(GOPATH_BIN)/govulncheck" || { \
		echo "govulncheck not found at $(GOPATH_BIN)/govulncheck; install it with:" >&2; \
		echo "  go install golang.org/x/vuln/cmd/govulncheck@latest" >&2; exit 1; }
	"$(GOPATH_BIN)/govulncheck" ./...

lint:
	@test -x "$(GOPATH_BIN)/staticcheck" || { \
		echo "staticcheck not found at $(GOPATH_BIN)/staticcheck; install it with:" >&2; \
		echo "  go install honnef.co/go/tools/cmd/staticcheck@latest" >&2; exit 1; }
	@test -x "$(GOPATH_BIN)/golangci-lint" || { \
		echo "golangci-lint not found at $(GOPATH_BIN)/golangci-lint; install it with:" >&2; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest" >&2; exit 1; }
	"$(GOPATH_BIN)/staticcheck" ./...
	"$(GOPATH_BIN)/golangci-lint" run

# publish pushes a sanitized single-commit snapshot of HEAD (README and
# code only, the 11 internal docs excluded) to the public GitHub remote
# through scripts/publish.sh. Arguments pass through PUBLISH_FLAGS, e.g.
# `make publish PUBLISH_FLAGS="--dry-run"`.
publish:
	./scripts/publish.sh $(PUBLISH_FLAGS)

# deb builds the Debian package (static release binary staged into
# dist/isopress_<version>_amd64.deb; version read from the binary's own
# --version). appimage stages an AppDir and packs it with appimagetool
# (dist/isopress-<version>-amd64.AppImage). Both write only under dist/
# (git-ignored) and are what .github/workflows/build.yml runs.
deb:
	./scripts/package-deb.sh

appimage:
	./scripts/package-appimage.sh

help:
	@echo "isopress make targets:"
	@echo "  build        build bin/isopress (default target)"
	@echo "  test         default fast suite (go test ./... -timeout 30m)"
	@echo "  vet          go vet ./..."
	@echo "  test-e2e     full suite incl. end-to-end tiers (-tags e2e, 30m)"
	@echo "  test-perf    performance smokes (-tags perf, 10m)"
	@echo "  test-race    full race gate (-tags e2e -race, 30m, -count=1)"
	@echo "  verify       build + vet x3 tags + gofmt check + go mod verify"
	@echo "  vulncheck    govulncheck ./... (Go bin directory)"
	@echo "  lint         staticcheck ./... + golangci-lint run (Go bin directory)"
	@echo "  publish      sanitized snapshot push to GitHub (scripts/publish.sh; PUBLISH_FLAGS=...)"
	@echo "  deb          dist/isopress_<version>_amd64.deb (scripts/package-deb.sh)"
	@echo "  appimage     dist/isopress-<version>-amd64.AppImage (scripts/package-appimage.sh)"
	@echo "  help         this list"
