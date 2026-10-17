# syntax=docker/dockerfile:1.19.0@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
# The echo agent's image: agent.py and the packages pinned, with hashes, in requirements.txt, on CPython 3.14, run as
# the same non-root user as Agent Kourier. Build it with `make echo-agent-image` from the repository root, or:
#
#   docker build -t agent-kourier-echo-agent:dev examples/recipes/echo-agent/agent
FROM python:3.14-slim@sha256:a2b82f3c48559aa0a8446d9af49826b6e2b2016f4cd2afabfe6013ec53729170
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app
COPY requirements.txt .
RUN pip install --require-hashes -r requirements.txt
COPY agent.py .
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["python", "agent.py"]
