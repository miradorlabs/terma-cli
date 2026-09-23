GO_IMAGE := golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b
IMAGE    := terma-funding-model
SEEDS    ?= 20
SCENARIO ?= all

# Every Go invocation runs in the pinned image; the host needs Docker only.
GO := docker run --rm -v "$(CURDIR)":/src -w /src \
       -v terma-funding-model-gocache:/root/.cache/go-build \
       -e CGO_ENABLED=0 -e GOTOOLCHAIN=local -e GOFLAGS=-mod=mod $(GO_IMAGE)

.PHONY: test fmt vet check build eval eval-json
test:
	$(GO) go test ./...
fmt:
	$(GO) gofmt -w .
vet:
	$(GO) go vet ./...
check: vet test
	$(GO) sh -c 'test -z "$$(gofmt -l .)"'
build:
	docker build -t $(IMAGE) .
eval: build
	docker run --rm --network none $(IMAGE) -scenario $(SCENARIO) -seeds $(SEEDS)
eval-json: build
	docker run --rm --network none $(IMAGE) -scenario $(SCENARIO) -seeds $(SEEDS) -json

# Paths are relative to this module and must be inside its Docker mount.
.PHONY: reconcile
reconcile:
	@$(GO) go run ./cmd/funding-reconcile -scope "$(SCOPE)" -report "$(REPORT)" -capture "$(CAPTURE)"
