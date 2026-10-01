# The E2E runner image: the test binary e2e-run.sh cross-compiles, on the distroless base that
# lib.sh pins (passed as a build argument with no default, so a build that forgets it fails).
ARG RUNTIME_IMAGE
FROM ${RUNTIME_IMAGE}
COPY e2e.test /
USER 65532:65532
ENTRYPOINT ["/e2e.test"]
