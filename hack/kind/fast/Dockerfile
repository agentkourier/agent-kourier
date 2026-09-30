# The fast E2E tier's test doubles in one image. up-fast.sh cross-compiles the three binaries on
# the host (GOOS=linux CGO_ENABLED=0) and builds with a directory holding just them as the
# context; each Deployment picks its binary with `command`. The base is the same digest-pinned
# distroless static image as up-full.sh's RUNTIME_IMAGE: keep the two in step.
FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY fakeslack stubagent sink /
USER 65532:65532
