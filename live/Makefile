# Live tests: real harness binaries, real provider credentials, the real terma
# binary, checked against the collection matrix. Built in the pinned Go image,
# run natively where the harnesses and their logins are.
GO_IMAGE := golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b
HOST_OS   := $(shell uname -s | tr A-Z a-z)
HOST_ARCH := $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
ROOT      := $(abspath ..)
GO := docker run --rm -v "$(ROOT)":/src -w /src/live \
       -v terma-cli-gocache:/root/.cache/go-build -v terma-cli-gomod:/go/pkg/mod \
       -e CGO_ENABLED=0 -e GOTOOLCHAIN=auto -e GOOS=$(HOST_OS) -e GOARCH=$(HOST_ARCH) $(GO_IMAGE)

.PHONY: build tidy vet test live run
tidy:
	$(GO) go mod tidy
vet:
	$(GO) sh -c 'gofmt -l . && go vet ./...'
# The terma binary the sandboxes call, and the test binary, both for this host.
build:
	$(GO) sh -c 'cd /src && go build -o live/bin/terma . && cd live && go test -c -o bin/live.test .'
# Offline contracts exercise malformed evidence without launching harnesses.
test: build
	TERMA_LIVE=0 ./bin/live.test -test.v -test.timeout 1m
# Loads live/.env.local (credentials, never committed) and runs every scenario.
# RUN=TestClaude... narrows it; ARGS adds test flags.
live: build
	@set -a; [ -f .env.local ] && . ./.env.local; set +a; \
	 TERMA_LIVE=1 TERMA_LIVE_BINARY="$(CURDIR)/bin/terma" PATH="$(CURDIR)/bin:$$PATH" \
	 ./bin/live.test -test.v -test.timeout 25m $(if $(RUN),-test.run '$(RUN)') $(ARGS); \
	 result=$$?; echo; echo "report: $(CURDIR)/report/latest.md"; exit $$result
