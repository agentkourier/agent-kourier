# syntax=docker/dockerfile:1.19.0@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# The prompt agent's image: agent.py, the example agent.yaml, and the packages pinned, with hashes, in requirements.txt
# (Pydantic AI with the OpenAI, Anthropic and Bedrock model clients and the MCP client), on CPython 3.14, run as the same
# non-root user as Agent Kourier. The Deployment in manifests/ mounts your own agent.yaml from a ConfigMap and points
# PROMPT_AGENT_SPEC at it. Build the image with `make prompt-agent-image` from the repository root, or:
#
#   docker build -t agent-kourier-prompt-agent:dev examples/recipes/prompt-agent/agent
#
# A release builds this file for linux/amd64 and linux/arm64 and publishes it as
# ghcr.io/agentkourier/agent-kourier-prompt-agent (hack/release/release.sh), after the same Trivy scan as Agent Kourier's
# image: no high or critical vulnerability, fixed or not. Hence Wolfi rather than a Debian-based Python image, and no pip
# in the final image. The base is pinned by digest; Wolfi's python-3.14 and py3.14-pip packages take their current
# releases when the image is built.
FROM cgr.dev/chainguard/wolfi-base:latest@sha256:833c48f1faf32c90e6f9501a2410a9aa28ceb2c594102edd4470053c30efa089 AS python
# python-3.14 (python-3.14-base too) depends on py3-pip-wheel, pip as a wheel for ensurepip. Its files are deleted in
# the same layer, so no layer of the final image holds pip; apk still lists the package, so the SBOM and Trivy name it,
# and an advisory against it fails the release scan until a rebuild takes Wolfi's fix.
RUN apk add --no-cache python-3.14 && rm -rf /usr/share/python-wheels

# The virtualenv is made here without pip, and Wolfi's pip installs into it from outside, so pip never enters it.
FROM python AS build
RUN apk add --no-cache py3.14-pip
COPY requirements.txt /tmp/requirements.txt
# Wheels only: pip runs no package's build code. Every pin has a wheel for CPython 3.14 on amd64 and arm64.
RUN python3.14 -m venv --without-pip /opt/venv \
 && python3.14 -m pip --python /opt/venv/bin/python install --no-cache-dir --disable-pip-version-check \
      --require-hashes --only-binary=:all: -r /tmp/requirements.txt

FROM python
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
COPY --from=build /opt/venv /opt/venv
WORKDIR /app
COPY agent.py agent.yaml ./
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/opt/venv/bin/python", "agent.py"]
