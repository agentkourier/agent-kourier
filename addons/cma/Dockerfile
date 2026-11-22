# The CMA add-on's image: the adapter (./cmd/adapter) built from this module, on the same pinned bases as Agent Kourier's
# image. GO_IMAGE and RUNTIME_IMAGE are the digest-pinned images in hack/kind/lib.sh and have no default, so a build
# that forgets them fails; `make cma-image` and hack/release/release.sh pass them. The build context is addons/, since the
# module (addons/cma) requires the shared kit (addons/kit) through `replace ../kit`; Dockerfile.dockerignore, beside this
# file, lets in only those two, without the nested livecheck module and the kind files. Build it as
# `docker build -f addons/cma/Dockerfile addons`. The final stage is distroless static, non-root, with the one binary.
#
# The Go build runs on the build host's own architecture and cross-compiles (GOOS and GOARCH from the platform
# arguments BuildKit sets), so a multi-platform build (hack/release/release.sh) never emulates the compiler. The module
# needs no cgo (its SQLite driver is pure Go), so the binary is static.
ARG GO_IMAGE
ARG RUNTIME_IMAGE

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
# VERSION becomes main.version: what --version prints, the agent card's version and the spans' service.version.
# hack/release/release.sh passes the release tag; a local build is dev.
ARG VERSION=dev
WORKDIR /src
COPY . .
WORKDIR /src/cma
# GOFLAGS is cleared so nothing in the environment can add build tags; GOENV=off goes with it, since cmd/go ignores an
# empty GOFLAGS= and falls back to a `go env -w` file (the root Dockerfile says the same).
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOFLAGS= GOENV=off CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/agent-kourier-cma ./cmd/adapter

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/agent-kourier-cma /agent-kourier-cma
USER 65532:65532
EXPOSE 8090
ENTRYPOINT ["/agent-kourier-cma"]
