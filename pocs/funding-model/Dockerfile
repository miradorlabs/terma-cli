# Same pinned toolchain the rest of this repository's PoCs build with. No module
# dependencies, so the build needs nothing beyond this image.
FROM golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS build
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=mod
WORKDIR /src
COPY . .
RUN test -z "$(gofmt -l .)" && go vet ./... && go test ./...
RUN go build -trimpath -o /out/funding-eval ./cmd/funding-eval

FROM scratch
COPY --from=build /out/funding-eval /funding-eval
ENTRYPOINT ["/funding-eval"]
