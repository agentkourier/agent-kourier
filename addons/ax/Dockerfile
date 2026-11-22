# The AX add-on's image: the agent (./cmd/agent) built from this module, on the same pinned bases as Agent Kourier's
# image. GO_IMAGE and RUNTIME_IMAGE are the digest-pinned images in hack/kind/lib.sh and have no default, so a build
# that forgets them fails; `make ax-image` and hack/release/release.sh pass them. The build context is this directory
# (addons/ax), a module of its own. The final stage is distroless static, non-root, with the one binary. A release
# publishes this image and the chart runs it; the route that runs the agent inside Google AX uses Dockerfile.ax
# (`make ax-runner-image`), which adds AX's task runner and git, and which no release builds.
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
# GOFLAGS is cleared so nothing in the environment can add build tags; GOENV=off goes with it, since cmd/go ignores an
# empty GOFLAGS= and falls back to a `go env -w` file (the root Dockerfile says the same).
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOFLAGS= GOENV=off CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/agent-kourier-ax ./cmd/agent

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/agent-kourier-ax /agent-kourier-ax
USER 65532:65532
EXPOSE 8090
ENTRYPOINT ["/agent-kourier-ax"]
