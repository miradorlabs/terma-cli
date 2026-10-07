# End-to-end tests: real harness binaries, real provider credentials, the real terma
# binary, checked against the collection matrix. Built in the pinned Go image,
# run natively where the harnesses and their logins are.
GO_IMAGE := golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b
HOST_OS   := $(shell uname -s | tr A-Z a-z)
HOST_ARCH := $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
ROOT      := $(abspath ../..)
GO := docker run --rm -v "$(ROOT)":/src -w /src/test/e2e \
       -v terma-cli-gocache:/root/.cache/go-build -v terma-cli-gomod:/go/pkg/mod \
       -e CGO_ENABLED=0 -e GOTOOLCHAIN=auto -e GOOS=$(HOST_OS) -e GOARCH=$(HOST_ARCH) $(GO_IMAGE)

.PHONY: build tidy vet test run telemetry
tidy:
	$(GO) go mod tidy
vet:
	$(GO) sh -c 'gofmt -l . && go vet ./...'
# The terma binary the sandboxes call, and the test binary, both for this host.
build:
	$(GO) sh -c 'cd /src && go build -o test/e2e/bin/terma ./cmd/terma && cd test/e2e && go test -c -o bin/e2e.test .'
# Offline contracts exercise malformed evidence without launching harnesses.
test: build
	TERMA_E2E=0 ./bin/e2e.test -test.v -test.timeout 1m

# Real harnesses, local fixture providers, no account credentials or paid calls.
telemetry: build
	TERMA_E2E=1 TERMA_E2E_BINARY="$(CURDIR)/bin/terma" \
	 ./bin/e2e.test -test.v -test.timeout 30m \
	 -test.run '^(TestClaudeTelemetry|TestCodexTelemetry|TestClaudeStopFailureDelivery|TestRelay.*|TestMachineHooks.*|TestPi.*|TestGemini.*|TestDsh.*|TestHermes.*)$$'
# Loads test/e2e/.env.local (credentials, never committed) and runs every scenario.
# RUN=TestClaude... narrows it; ARGS adds test flags.
run: build
	@set -a; [ -f .env.local ] && . ./.env.local; set +a; \
	 TERMA_E2E=1 TERMA_E2E_BINARY="$(CURDIR)/bin/terma" PATH="$(CURDIR)/bin:$$PATH" \
	 ./bin/e2e.test -test.v -test.timeout 25m $(if $(RUN),-test.run '$(RUN)') $(ARGS); \
	 result=$$?; echo; echo "report: $(CURDIR)/report/latest.md"; exit $$result

# The machine scenarios on a fresh Linux machine (test/e2e/machine/Dockerfile): global mode
# and the core relay contracts, with real Claude Code and Codex, as a developer's
# machine would run them. terma and the test binary are built for Linux here and
# mounted in. CLAUDE_VERSION / CODEX_VERSION pick the agents; RUN narrows the scenarios.
MACHINE_IMAGE   := terma-e2e-machine
CLAUDE_VERSION  ?= 2.1.285
CODEX_VERSION   ?= 0.158.0
MACHINE_RUN     ?= ^(TestGlobalMode.*|TestRelayClaude|TestRelayCodex|TestRelayNegativeControls|TestRelayResumedElsewhere)$$
LINUX_GO := docker run --rm -v "$(ROOT)":/src -w /src/test/e2e \
       -v terma-cli-gocache:/root/.cache/go-build -v terma-cli-gomod:/go/pkg/mod \
       -e CGO_ENABLED=0 -e GOTOOLCHAIN=auto -e GOOS=linux -e GOARCH=$(HOST_ARCH) $(GO_IMAGE)
.PHONY: machines machine-image
machine-image:
	docker build --build-arg CLAUDE_VERSION=$(CLAUDE_VERSION) --build-arg CODEX_VERSION=$(CODEX_VERSION) \
	 -t $(MACHINE_IMAGE):$(CLAUDE_VERSION)-$(CODEX_VERSION) machine
machines: machine-image
	# -buildvcs=false: git rejects the mounted repo's owner inside the container, and this
	# test binary carries no version anyway.
	$(LINUX_GO) sh -c 'cd /src && go build -buildvcs=false -o test/e2e/bin/linux/terma ./cmd/terma && cd test/e2e && go test -c -o bin/linux/e2e.test .'
	# seccomp unconfined: Codex runs tool commands in bwrap, which needs the user
	# namespaces Docker's default profile forbids and a developer's machine allows.
	docker run --rm --security-opt seccomp=unconfined -v "$(CURDIR)":/work/e2e -w /work/e2e \
	 -e TERMA_E2E=1 -e TERMA_ENV=dev -e TERMA_E2E_BINARY=/work/e2e/bin/linux/terma \
	 -e TERMA_E2E_CLAUDE_VERSIONS=installed -e TERMA_E2E_CODEX_VERSIONS=installed \
	 -e TERMA_E2E_KNOWN_UPSTREAM=codex-session-end -e TERMA_E2E_REPORT=report/linux \
	 $(MACHINE_IMAGE):$(CLAUDE_VERSION)-$(CODEX_VERSION) \
	 ./bin/linux/e2e.test -test.v -test.timeout 40m -test.run '$(if $(RUN),$(RUN),$(MACHINE_RUN))'

# The compatibility matrix (docs/COMPATIBILITY.md, docs/compat/): merge the latest runs'
# report/compat.json — this machine's, and the Linux machine's from `make machines` —
# into the history and render it. RUNS names other runs' files.
RUNS ?= $(wildcard report/compat.json report/linux/compat.json)
.PHONY: compat
compat:
	go run ./compatgen $(foreach r,$(RUNS),-run $(r))
