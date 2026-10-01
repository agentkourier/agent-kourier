# courier's image: a multi-stage build from pinned base images, the same on every host. GO_IMAGE and
# RUNTIME_IMAGE are the digest-pinned images in hack/kind/lib.sh and have no default, so a build that
# forgets them fails. TAGS is the Go build tags: empty for the production image, and "testhooks" for
# the image the kind tiers run (see internal/testhooks), which is never pushed and carries a label
# (hack/kind/lib.sh adds it). The final stage is distroless static, non-root, with the one binary.
ARG GO_IMAGE
ARG RUNTIME_IMAGE

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY . .
ARG TAGS=
ARG VERSION=dev
# GOFLAGS is cleared so nothing in the environment can add build tags to the production build. GOENV=off
# goes with it: cmd/go ignores an empty GOFLAGS= and falls back to a `go env -w` file (cmd/courier's
# binary_test.go proves both).
# No -s -w: the image test (internal/testhooks/image_test.go) reads symbols with `go tool nm`, and a
# stripped binary has none, which would blind its check of the crash function.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOFLAGS= GOENV=off CGO_ENABLED=0 go build -trimpath -tags "${TAGS}" -ldflags "-X main.version=${VERSION}" -o /out/courier ./cmd/courier

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/courier /courier
USER 65532:65532
ENTRYPOINT ["/courier"]
