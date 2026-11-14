# The prompt agent's two test doubles in the fast tier, built from the recipe's directory (examples/recipes/prompt-agent/agent) as the
# context, one target each (lib.sh's deploy_prompt_agent): the fake model (fake_model.py, the standard library only) and the test MCP
# server (mcp_server.py, with FastMCP from requirements-dev.txt, hashed). Neither is the recipe's image, which the recipe's own Dockerfile
# builds. Both run on the recipe's Python image as the same non-root user: PYTHON_IMAGE is the digest-pinned image of the recipe's
# Dockerfile, which deploy_prompt_agent reads and passes as a build argument, so the pin is kept in one place; it has no default, so a
# build that forgets it fails.
ARG PYTHON_IMAGE
FROM ${PYTHON_IMAGE} AS fake-model
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
WORKDIR /app
COPY fake_model.py ./
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["python", "fake_model.py", "8080"]

FROM ${PYTHON_IMAGE} AS mcp
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app
COPY requirements-dev.txt ./
# Wheels only, as in the recipe's image: pip runs no package's build code.
RUN pip install --require-hashes --only-binary=:all: -r requirements-dev.txt
COPY mcp_server.py ./
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["python", "mcp_server.py", "8080"]
