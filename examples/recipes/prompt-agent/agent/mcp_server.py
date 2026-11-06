"""A tiny Streamable HTTP MCP server for the tests: three tools, a bearer check, and a call log.

MCP_TOKEN (env) is the bearer it requires; MCP_CALL_LOG (env) is a JSONL file of every tool event, so a test can
see what reached the server, when, and whether a call was cancelled. The tests start it as a subprocess on a free port.
"""

import asyncio
import hmac
import json
import os
import sys
import time

import uvicorn
from fastmcp import FastMCP
from starlette.middleware import Middleware
from starlette.responses import PlainTextResponse

mcp = FastMCP("ops")
LOG = os.environ["MCP_CALL_LOG"]


def log(**event) -> None:
    with open(LOG, "a") as f:
        f.write(json.dumps({"t": time.time(), **event}) + "\n")


@mcp.tool
def runbook(alert: str) -> str:
    """Return the runbook for an alert name."""
    log(tool="runbook", phase="call", alert=alert)
    return f"Runbook for {alert}: check the pod's recent restarts, then its memory limit."


@mcp.tool
async def slow_check(seconds: float) -> str:
    """Run a health check that takes the given number of seconds."""
    log(tool="slow_check", phase="start", seconds=seconds)
    try:
        await asyncio.sleep(seconds)
    except asyncio.CancelledError:  # the MCP client sent a cancellation: the tests assert this line appears
        log(tool="slow_check", phase="cancelled")
        raise
    log(tool="slow_check", phase="done")
    return "healthy"


@mcp.tool
def restart_cluster() -> str:
    """Restart every node in the production cluster. Never in allowed_tools: the tests assert it is never called."""
    log(tool="restart_cluster", phase="call")
    return "restarted"


class RequireBearer:
    """Answers 401 to a request without the right bearer, and logs it, so a test can tell it was refused."""

    def __init__(self, app) -> None:
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope["type"] == "http":
            given = dict(scope["headers"]).get(b"authorization", b"")
            if not hmac.compare_digest(given, b"Bearer " + os.environ["MCP_TOKEN"].encode()):
                log(tool=None, phase="unauthorized")
                return await PlainTextResponse("unauthorized", status_code=401)(scope, receive, send)
        await self.app(scope, receive, send)


app = mcp.http_app(path="/mcp", transport="streamable-http", middleware=[Middleware(RequireBearer)])

if __name__ == "__main__":
    uvicorn.run(app, host="127.0.0.1", port=int(sys.argv[1]), log_level="warning")
