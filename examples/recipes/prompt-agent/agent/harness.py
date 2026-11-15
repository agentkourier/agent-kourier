"""Helpers for the tests: the agent behind an in-process ASGI transport, a send like the broker's, the test MCP server.

The model is a scripted FunctionModel (no network, no key, no spend). The MCP server is the real one in
mcp_server.py, run as a subprocess, because the questions are about what really crosses the MCP wire: the header, the
tool list, a cancel.
"""

import asyncio
import contextlib
import json
import os
import socket
import subprocess
import sys
import time
import uuid
from pathlib import Path

import httpx
import yaml
from a2a.client import ClientConfig, ClientFactory
from a2a.types.a2a_pb2 import GetTaskRequest, ListTasksRequest, Message, Part, Role, SendMessageRequest, TaskState

import agent

HERE = Path(__file__).parent
URL = "http://prompt-agent.test"
TOKEN = "fake-mcp-token"  # not a secret: the bearer the test MCP server is started with
BEARER = "fake-binding-token"  # not a secret: the bearer the agent itself requires, as the Binding's token would be
DONE, WORKING, FAILED = TaskState.TASK_STATE_COMPLETED, TaskState.TASK_STATE_WORKING, TaskState.TASK_STATE_FAILED
CANCELED, INPUT = TaskState.TASK_STATE_CANCELED, TaskState.TASK_STATE_INPUT_REQUIRED


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_for_port(port: int) -> None:
    """Waits up to about 10 seconds for a subprocess to listen on port."""
    for _ in range(200):
        with contextlib.suppress(OSError), socket.create_connection(("127.0.0.1", port), 0.1):
            return
        time.sleep(0.05)


@contextlib.contextmanager
def mcp_server(directory: Path):
    """Starts mcp_server.py with TOKEN as its bearer; yields its URL and a function that reads its call log."""
    port, log = free_port(), directory / "mcp-calls.jsonl"
    env = {**os.environ, "MCP_TOKEN": TOKEN, "MCP_CALL_LOG": str(log)}
    proc = subprocess.Popen([sys.executable, str(HERE / "mcp_server.py"), str(port)], env=env)  # noqa: S603
    try:
        wait_for_port(port)
        yield (
            f"http://127.0.0.1:{port}/mcp",
            lambda: [json.loads(x) for x in log.read_text().splitlines()] if log.exists() else [],
        )
    finally:
        proc.terminate()
        proc.wait()


@contextlib.contextmanager
def fake_model():
    """Starts fake_model.py; yields the base URL an OpenAI client takes (OPENAI_BASE_URL)."""
    port = free_port()
    proc = subprocess.Popen([sys.executable, str(HERE / "fake_model.py"), str(port)])  # noqa: S603
    try:
        wait_for_port(port)
        yield f"http://127.0.0.1:{port}/v1"
    finally:
        proc.terminate()
        proc.wait()


def spec_file(directory: Path, url: str, model: str = "test", **mcp) -> str:
    """Writes an agent.yaml for the test server; returns its path. `mcp` overrides the MCP arguments (None drops).

    The default model is a placeholder that tests replace with their own; a test that names a real one (the fake
    model's) passes no model of its own, and the spec's is built and checked as in the pod.
    """
    args = {
        "url": url,
        "authorization_token": "Bearer ${OPS_MCP_TOKEN}",
        "allowed_tools": ["runbook", "slow_check"],
        **mcp,
    }
    spec = {
        "model": model,
        "name": "ops-helper",
        "description": "Answers questions about alerts.",
        "instructions": "You help on-call engineers.",
        "capabilities": [{"MCP": {k: v for k, v in args.items() if v is not None}}],
    }
    path = directory / "agent.yaml"
    path.write_text(yaml.safe_dump(spec))
    return str(path)


@contextlib.asynccontextmanager
async def asgi(spec_path, model, token_file=None, headers=None, anonymous=""):
    """The app behind an in-process ASGI transport: yields a raw HTTP client.

    With no token_file the agent requires BEARER, from a file next to the spec, and the client sends it, as the
    Deployment and the Binding do. A test of the bearer check passes its own file and headers.
    """
    if token_file is None:
        token_file = str(Path(spec_path).parent / "bearer")
        Path(token_file).write_text(BEARER + "\n")
        headers = {"Authorization": "Bearer " + BEARER, **(headers or {})}
    transport = httpx.ASGITransport(app=agent.build_app(URL, spec_path, token_file, model, anonymous))
    async with httpx.AsyncClient(transport=transport, base_url=URL, headers=headers, timeout=60) as http:
        yield http


@contextlib.asynccontextmanager
async def serve(spec_path, model):
    """Yields the SDK's A2A client over the app."""
    async with asgi(spec_path, model) as http:
        yield await ClientFactory(ClientConfig(httpx_client=http)).create_from_url(URL)


def message(text, context_id="", task_id="", message_id="", metadata=None):
    """A user message like the broker's: one text part."""
    m = Message(message_id=message_id or str(uuid.uuid4()), role=Role.ROLE_USER, parts=[Part(text=text)])
    m.context_id, m.task_id = context_id, task_id
    if metadata:
        m.metadata.update(metadata)
    return SendMessageRequest(message=m)


async def say(client, *args, **kwargs):
    """One send. Returns the stream's events and the task as GetTask reads it afterwards.

    The SDK ends a stream with a status update, not a Task, and so does this agent; Agent Kourier settles that with one
    GetTask, and so does this. The ASGI transport hands over the response only when it is complete, so a test sees the
    events of a stream after it ends, not while it runs.
    """
    events = [r async for r in client.send_message(message(*args, **kwargs))]
    first = events[0]  # a new task starts with the Task; an answer to a paused one starts with a status update
    task_id = first.task.id if first.HasField("task") else getattr(first, first.WhichOneof("payload")).task_id
    return events, await client.get_task(GetTaskRequest(id=task_id))


def artifact_text(task):
    """What Agent Kourier would show: the artifact text parts, joined."""
    return "".join(part.text for artifact in task.artifacts for part in artifact.parts)


def working(events):
    """The `working` status updates in a stream."""
    return [e for e in events if e.HasField("status_update") and e.status_update.status.state == WORKING]


async def until(condition, what="the condition"):
    """Waits (in loop turns, with a short real sleep) until condition() holds."""
    for _ in range(1000):
        if condition():
            return
        await asyncio.sleep(0.01)
    raise AssertionError(f"timed out waiting for {what}")


async def running_task(client):
    """The task that is working, once there is one."""
    for _ in range(500):
        tasks = (await client.list_tasks(ListTasksRequest())).tasks
        if any(t.status.state == WORKING for t in tasks):
            return next(t for t in tasks if t.status.state == WORKING)
        await asyncio.sleep(0.01)
    raise AssertionError("no task started working")
