# Agent Kourier's image: a multi-stage build from pinned base images, the same on every host. GO_IMAGE and
# RUNTIME_IMAGE are the digest-pinned images in hack/kind/lib.sh and have no default, so a build that
# forgets them fails. TAGS is the Go build tags: empty for the production image, and "testhooks" for
# the image the kind tiers run (see internal/testhooks), which is never pushed and carries a label
# (hack/kind/lib.sh adds it). The final stage is distroless static, non-root, with the one binary.
#
# The Go build runs on the build host's own architecture and cross-compiles (GOOS and GOARCH from the
# platform arguments BuildKit sets), so a multi-platform build (hack/release/release.sh) never emulates
# the compiler. A plain `docker build` targets the host's platform and behaves as it always did.
ARG GO_IMAGE
ARG RUNTIME_IMAGE

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY . .
ARG TAGS=
ARG VERSION=dev
# GOFLAGS is cleared so nothing in the environment can add build tags to the production build. GOENV=off
# goes with it: cmd/go ignores an empty GOFLAGS= and falls back to a `go env -w` file (cmd/agent-kourier's
# binary_test.go proves both).
# No -s -w: the image test (internal/testhooks/image_test.go) reads symbols with `go tool nm`, and a
# stripped binary has none, which would blind its check of the crash function.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOFLAGS= GOENV=off CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -tags "${TAGS}" -ldflags "-X main.version=${VERSION}" -o /out/agent-kourier ./cmd/agent-kourier

# verify refuses to go on if the binary is not the one to ship, for the platform being built: the final
# stage copies the binary from here, so no production image of any architecture exists without passing.
# It runs two tests of internal/testhooks on the build host against the cross-compiled binary, which it
# cannot run: the one `make image-check` shares (no test hook in it, with a testhooks build of the same
# platform as the control), and one that it is an ELF of this GOARCH with main.version set to VERSION.
# It requires each test's own PASS line, so a renamed test ("no tests to run" exits 0) fails the build.
# Only TAGS exactly "testhooks", the kind tiers' image, skips it: that build has the hooks by design.
FROM build AS verify
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build <<'EOF'
set -eu
[ "${TAGS}" != testhooks ] || exit 0
export GOFLAGS= GOENV=off CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}"
go build -trimpath -tags testhooks -o /tmp/agent-kourier-hooked ./cmd/agent-kourier
AGENTKOURIER_BINARY=/out/agent-kourier AGENTKOURIER_HOOKED_BINARY=/tmp/agent-kourier-hooked AGENTKOURIER_VERSION="${VERSION}" AGENTKOURIER_GOARCH="${TARGETARCH}" \
  go test -v -tags image -count=1 -run '^TestBinary(HasNoHooks|Identity)$' ./internal/testhooks >/tmp/verify.log 2>&1 || { cat /tmp/verify.log; exit 1; }
cat /tmp/verify.log
for t in TestBinaryHasNoHooks TestBinaryIdentity; do
  grep -q "^--- PASS: ${t} " /tmp/verify.log || { echo "verify: ${t} did not run and pass" >&2; exit 1; }
done
EOF

FROM ${RUNTIME_IMAGE}
COPY --from=verify /out/agent-kourier /agent-kourier
USER 65532:65532
ENTRYPOINT ["/agent-kourier"]
