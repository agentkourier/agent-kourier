# The fast E2E tier's test doubles in one image. up-fast.sh cross-compiles the four binaries on
# the host (GOOS=linux CGO_ENABLED=0) and builds with a directory holding only them as the
# context; each Deployment picks its binary with `command`. RUNTIME_IMAGE is the digest-pinned
# distroless static image in lib.sh, passed as a build argument so that up-full.sh and up-fast.sh
# share one pin; it has no default, so a build that forgets it fails.
ARG RUNTIME_IMAGE
FROM ${RUNTIME_IMAGE}
COPY fakeslack stubagent sink signaler hitlagent /
USER 65532:65532
