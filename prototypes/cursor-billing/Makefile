GO_IMAGE := golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b
GO := docker run --rm -v "$(CURDIR)":/src -w /src -v terma-cursor-billing-gocache:/root/.cache/go-build $(GO_IMAGE)
HOST_OS := $(shell uname -s | tr A-Z a-z)
HOST_ARCH := $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
.PHONY: fmt check build
fmt:
	$(GO) gofmt -w .
check:
	$(GO) sh -c 'test -z "$$(gofmt -l .)" && go vet ./... && go test -race ./...'
build:
	$(GO) env CGO_ENABLED=0 GOOS=$(HOST_OS) GOARCH=$(HOST_ARCH) go build -o bin/cursor-billing-import ./cmd/cursor-billing-import
