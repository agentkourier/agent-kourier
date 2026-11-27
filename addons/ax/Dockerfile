# The AX launcher image uses the pinned bases in hack/kind/lib.sh. The build
# context is addons/ because the AX module replaces the kit with ../kit.
#
# The Go build runs on the build host's own architecture and cross-compiles (GOOS and GOARCH from the platform
# arguments BuildKit sets), so a multi-platform build (hack/release/release.sh) never emulates the compiler. The module
# needs no cgo (its SQLite driver is pure Go), so the binary is static.
ARG GO_IMAGE
ARG RUNTIME_IMAGE

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
# VERSION becomes main.version. hack/release/release.sh passes the release tag; a local build is dev.
ARG VERSION=dev
WORKDIR /src
COPY . .
WORKDIR /src/ax
# GOFLAGS is cleared so nothing in the environment can add build tags; GOENV=off goes with it, since cmd/go ignores an
# empty GOFLAGS= and falls back to a `go env -w` file (the root Dockerfile says the same).
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOFLAGS= GOENV=off CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/agent-kourier-ax ./cmd/agent && \
    GOFLAGS= GOENV=off CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -o /out/agent-kourier-ax-result ./cmd/agent-kourier-ax-result

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/agent-kourier-ax /agent-kourier-ax
COPY --from=build /out/agent-kourier-ax-result /usr/local/bin/agent-kourier-ax-result
USER 65532:65532
EXPOSE 8090
ENTRYPOINT ["/agent-kourier-ax"]
