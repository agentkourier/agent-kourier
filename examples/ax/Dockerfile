FROM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOMAXPROCS=2 go build -p 2 -trimpath -o /out/ax-task-runner ./cmd/ax-task-runner
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOMAXPROCS=2 go build -p 2 -trimpath -o /out/agent-kourier-ax-agent ./cmd/agent

# Public runtime with Git and CA roots for AX workspace preparation.
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ax-task-runner /usr/local/bin/ax-task-runner
COPY --from=build /out/agent-kourier-ax-agent /usr/local/bin/agent-kourier-ax-agent
ENTRYPOINT ["/usr/local/bin/ax-task-runner"]
