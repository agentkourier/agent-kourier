# syntax=docker/dockerfile:1.19.0@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# The docs agent's image: the agent binary and the documentation it answers from, in a distroless, non-root image.
#
# The build context is this directory. The documentation is a second, named context, so that it can come from
# anywhere (the repository's docs/ here, your own knowledge base in your copy of the recipe):
#
#   docker build --build-context docs=docs -t agent-kourier-docs-agent:dev examples/recipes/docs-agent/agent
#
# The repository's docs/ also holds the brand kit and the spike notes, which are not published and must not be answered
# from, and the brand kit is large. Two things keep them out, and neither does the other's job:
#
#   - the COPY below excludes both directories, so they never reach a layer, whatever the context holds;
#   - docs/.dockerignore lists both, so that the builder is never sent them. BuildKit sends the whole of a named
#     context unless the context has a .dockerignore; COPY --exclude does not narrow what is sent (measured with
#     Docker 29.8, BuildKit 0.33: 300 MB of brand files were sent with the exclude alone, and a few kB with the
#     .dockerignore).
#
# The syntax line above names the Dockerfile frontend by version and digest, as the base images below are named. COPY
# --exclude needs Dockerfile syntax 1.19 (Docker's Dockerfile reference); with the line, a BuildKit builder pulls that
# frontend, so the version of the Docker daemon does not decide whether the flag works.
#
# CORPUS_REMOVE names further directories to take out afterwards, such as "drafts" for your own documentation; give
# your own documentation context a .dockerignore of its own to keep them from being sent. Only Markdown files go into
# the image.
FROM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -o /out/docs-agent ./cmd/docs-agent

FROM build AS corpus
ARG CORPUS_REMOVE=""
COPY --from=docs --exclude=brand --exclude=spikes . /corpus
RUN set -eu; cd /corpus; \
    for d in ${CORPUS_REMOVE}; do rm -rf -- "./${d}"; done; \
    find . -type f ! -name '*.md' -delete; \
    find . -mindepth 1 -type d -empty -delete; \
    test -n "$(find . -name '*.md' -print -quit)" || { echo "the corpus holds no Markdown file" >&2; exit 1; }

FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY --from=build /out/docs-agent /docs-agent
COPY --from=corpus /corpus /corpus
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/docs-agent"]
