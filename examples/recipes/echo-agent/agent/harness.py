"""Helpers for the tests: the agent behind an in-process ASGI transport, a send like the broker's, raw JSON-RPC."""

import asyncio
import contextlib
import uuid

import httpx
from a2a.client import ClientConfig, ClientFactory
from a2a.types.a2a_pb2 import ListTasksRequest, Message, Part, Role, SendMessageRequest, TaskState

import agent

URL = "http://echo-agent.test"
DONE, WORKING = TaskState.TASK_STATE_COMPLETED, TaskState.TASK_STATE_WORKING
FAILED, CANCELED = TaskState.TASK_STATE_FAILED, TaskState.TASK_STATE_CANCELED
INPUT = TaskState.TASK_STATE_INPUT_REQUIRED


@contextlib.asynccontextmanager
async def asgi(token_file="", headers=None):
    """The app behind an in-process ASGI transport: yields a raw HTTP client."""
    transport = httpx.ASGITransport(app=agent.build_app(URL, token_file))
    async with httpx.AsyncClient(transport=transport, base_url=URL, headers=headers) as http:
        yield http


@contextlib.asynccontextmanager
async def serve():
    """Yields the raw HTTP client and the SDK's A2A client over it."""
    async with asgi() as http:
        yield http, await ClientFactory(ClientConfig(httpx_client=http)).create_from_url(URL)


async def collect(stream):
    return [r async for r in stream]


async def say(client, text, context_id="", task_id="", metadata=None):
    """One send, like the broker's: a user message with one text part. Returns the stream's responses."""
    message = Message(message_id=str(uuid.uuid4()), role=Role.ROLE_USER, parts=[Part(text=text)])
    message.context_id, message.task_id = context_id, task_id
    if metadata:
        message.metadata.update(metadata)
    return [r async for r in client.send_message(SendMessageRequest(message=message))]


def final(events):
    """The stream's last event, which must be a full Task."""
    assert events[-1].HasField("task"), f"the stream did not end with a Task: {events[-1]}"
    return events[-1].task


def artifact_text(task):
    """What Agent Kourier would show: the artifact text parts, joined."""
    return "".join(part.text for artifact in task.artifacts for part in artifact.parts)


async def rpc(http, method, params):
    """A raw JSON-RPC call: returns the HTTP response, to see the error code on the wire."""
    body = {"jsonrpc": "2.0", "id": str(uuid.uuid4()), "method": method, "params": params}
    return await http.post("/", json=body, headers={"A2A-Version": "1.0"})


def error_code(response):
    assert response.headers["content-type"].startswith("application/json"), response.text
    return response.json()["error"]["code"]


async def running_task(client):
    """Wait until a task is working, so a test can act in the middle of its stream."""
    for _ in range(200):
        tasks = (await client.list_tasks(ListTasksRequest())).tasks
        if any(t.status.state == WORKING for t in tasks):
            return next(t for t in tasks if t.status.state == WORKING)
        await asyncio.sleep(0.01)
    raise AssertionError("no task started working")
