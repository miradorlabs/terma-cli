BINARY := bin/terma
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/miradorlabs/terma-cli/cmd.Version=$(VERSION)

# Everything here that runs terma's code runs it against the dev environment. `terma
# install` and `terma setup` sign in, so a test or a script that reaches them would
# otherwise open a browser login on production. Override with `TERMA_ENV=… make test`.
export TERMA_ENV ?= dev

# golangci-lint is pinned in one file, which CI's action reads too, and built from source
# with this machine's Go. A released binary refuses a module whose `go` directive is newer
# than the Go it was built with — which is how `make lint` stopped working on the day
# go.mod moved to a release the Homebrew bottle had not caught up with.
GOLANGCI_LINT_VERSION := $(shell cat .golangci-lint-version)
GOLANGCI_LINT := bin/golangci-lint-$(GOLANGCI_LINT_VERSION)

.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

# Install onto PATH as `terma`. Plain `go install` would name it `terma-cli`
# after the module path, so the binary is placed explicitly.
.PHONY: install
install:
	go build -ldflags "$(LDFLAGS)" -o "$(shell go env GOPATH)/bin/terma" .
	@echo "installed $(shell go env GOPATH)/bin/terma"
	@command -v terma >/dev/null 2>&1 || echo "note: $(shell go env GOPATH)/bin is not on your PATH"

.PHONY: test
test:
	go test ./...

# Builds and exercises the real CLI in isolated workspaces; no login or live backend.
.PHONY: test-install-e2e
test-install-e2e:
	go test ./cmd -run '^TestInstallE2E' -count=1 -v

.PHONY: cover
cover:
	go test -cover ./...

.PHONY: fmt
fmt:
	gofmt -w .

# What CI checks: nothing is rewritten, so `make check` is safe to run before a commit
# without it changing what is being committed.
.PHONY: fmt-check
fmt-check:
	@unformatted="$$(gofmt -l . 2>/dev/null | grep -v '^dist/' || true)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed on:"; echo "$$unformatted"; exit 1; fi

.PHONY: vet
vet:
	go vet ./...

# Apply go fix across the module (e.g. interface{} → any modernization). Also runs
# per-package as you edit, via the PostToolUse hook (.claude/hooks/go-fix.sh).
.PHONY: fix
fix:
	go fix ./...

# Format Go code: go fix modernizers first, then gofmt reconciles.
.PHONY: format
format: fix
	gofmt -w .

# Run golangci-lint (configured in .golangci.yml) — the version CI runs, built on first
# use (a minute or two, once per version). Also runs per-package as you edit, via the
# PostToolUse hook (.claude/hooks/lint-go.sh), which uses the same binary.
.PHONY: lint
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...

$(GOLANGCI_LINT): .golangci-lint-version
	@echo "building golangci-lint $(GOLANGCI_LINT_VERSION) with $$(go env GOVERSION)"
	@mkdir -p bin
	GOBIN="$(CURDIR)/bin/.lint-$(GOLANGCI_LINT_VERSION)" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv "bin/.lint-$(GOLANGCI_LINT_VERSION)/golangci-lint" "$@" && rmdir "bin/.lint-$(GOLANGCI_LINT_VERSION)"

# The OpenCode plugin is JavaScript; Bun runs it the way OpenCode does. Skipped when
# bun is not installed, so `make check` still works on a Go-only machine.
.PHONY: test-plugin
test-plugin:
	@if command -v bun >/dev/null 2>&1; then (cd internal/harness/opencode && bun test); \
	else echo "bun not installed; skipping OpenCode plugin tests"; fi

.PHONY: check
check: fmt-check vet lint test test-plugin

# Hook budget: prepare-commit-msg must stay well under 50 ms end to end. Runs the
# installed shim against a scratch repository and prints the wall time.
.PHONY: bench-hook
bench-hook: build
	@./scripts/bench-hook.sh

# Build the release archives locally without publishing — same path CI takes. The
# tap token is only read by the cask template; any value renders it.
.PHONY: release-dry-run
release-dry-run:
	HOMEBREW_TAP_TOKEN="$${HOMEBREW_TAP_TOKEN:-unset}" \
		go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=publish

# What CI runs before a release can ship: render dist/ from a throwaway local tag
# (so the cask URL carries v#{version} exactly as a real release would), then
# install it with install.sh and — on macOS — with Homebrew, both over loopback.
.PHONY: test-install
test-install:
	git tag -f v0.0.0-ci >/dev/null
	HOMEBREW_TAP_TOKEN="$${HOMEBREW_TAP_TOKEN:-unset}" \
		go run github.com/goreleaser/goreleaser/v2@latest release --clean --skip=publish,validate,announce,before; \
		status=$$?; git tag -d v0.0.0-ci >/dev/null; [ $$status -eq 0 ]
	./scripts/test-install.sh dist v0.0.0-ci
	@if [ "$$(uname -s)" = Darwin ]; then ./scripts/test-cask.sh dist v0.0.0-ci; \
	else echo "not macOS; skipping the cask install"; fi

# The linter under bin/ is kept: it takes a minute or two to rebuild, and the edit-time
# hook goes quiet without it rather than failing, so nothing would say it had stopped.
.PHONY: clean
clean:
	rm -rf $(BINARY) dist

# Cross-compiled release binaries. CGO is off so each one is a static binary that
# runs on any machine of its platform without a matching libc.
.PHONY: dist
dist:
	@mkdir -p dist
	@for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "building dist/terma-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" \
			-o dist/terma-$$os-$$arch$$ext . || exit 1; \
	done
