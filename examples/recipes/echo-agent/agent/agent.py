"""A plain A2A agent for Agent Kourier, with no model: read it top to bottom to see what your agent must do.

Each block names the row of docs/reference/agent-contract.md it satisfies. The a2a-sdk serves the protocol (JSON-RPC,
streaming, GetTask, ListTasks, SubscribeToTask, CancelTask), so this file holds only the agent's own behaviour.
Say `ask`, `fail` or `help` to try the paths a real agent has too; anything else is echoed back.
"""

import asyncio
import hmac
import math
import os
import re
from collections import OrderedDict

import uvicorn
from a2a.helpers import new_task_from_user_message, new_text_part
from a2a.server.agent_execution import AgentExecutor, RequestContext
from a2a.server.events import EventQueue
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.routes import create_agent_card_routes, create_jsonrpc_routes
from a2a.server.tasks import InMemoryTaskStore, TaskUpdater
from a2a.types.a2a_pb2 import (
    AgentCapabilities,
    AgentCard,
    AgentInterface,
    AgentSkill,
    GetTaskRequest,
    TaskState,
    TaskStatusUpdateEvent,
)
from starlette.applications import Starlette
from starlette.responses import PlainTextResponse

CHUNKS, CHUNK_DELAY = 8, 0.25  # a reply streams in 8 pieces over about two seconds, so Slack's live update shows
MAX_CONTEXTS = 1000  # conversations remembered; the oldest is forgotten first

HELP = "Say `ask` and I will ask you a question, `fail` and I will fail, anything else and I will echo it back."


def build_card(url: str) -> AgentCard:
    """The agent card. Contract, Discovery: streaming on, one JSON-RPC interface, no required extension."""
    return AgentCard(
        name="echo-agent",
        description="Echoes what it is told, streams the reply, and can ask a question or fail on request.",
        version="1.0.0",
        supported_interfaces=[AgentInterface(url=url, protocol_binding="JSONRPC", protocol_version="1.0")],
        capabilities=AgentCapabilities(streaming=True),  # Agent Kourier never uses the non-streaming send
        default_input_modes=["text/plain"],
        default_output_modes=["text/plain"],
        skills=[AgentSkill(id="echo", name="Echo", description=HELP, tags=["demo"])],
    )


def words_of(context: RequestContext) -> str:
    """The person's words, without the prefix and the bot mention Agent Kourier puts in front of them."""
    text = context.get_user_input()
    if "agentkourier.dev/actor" in context.message.metadata:  # Contract, What Agent Kourier sends: a person's turn
        text = text.split("\n", 1)[-1]  # starts with "[Slack: Name (U123) <email>]" and a newline
    return re.sub(r"<@[^>]+>", "", text).strip()


class EchoExecutor(AgentExecutor):
    """One A2A task per turn: execute() runs it and cancel() stops it."""

    def __init__(self) -> None:
        self.turns: OrderedDict[str, tuple[int, str]] = OrderedDict()  # contextId -> (turn number, last words)

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        # Contract, Replies: every event names the task and its contextId, and the first event is the Task itself.
        # The SDK made both ids. A thread's first turn arrives with no contextId; later turns carry the one we sent.
        if context.current_task is None:
            await event_queue.enqueue_event(new_task_from_user_message(context.message))
        task = TaskUpdater(event_queue, context.task_id, context.context_id)
        words = words_of(context)
        await task.start_work()

        # Contract, Task states: a question is `input-required` with a status message of text parts only. Agent
        # Kourier sends the answer to this same task, so execute() runs again with the paused task as current_task.
        if context.current_task and context.current_task.status.state == TaskState.TASK_STATE_INPUT_REQUIRED:
            return await self.reply(task, f"You answered: {words}")

        # Contract, Replies: a thread keeps its contextId, so remember what was said in it. Agent Kourier may send a
        # turn twice with the same messageId; echoing twice is harmless, but an agent with side effects must drop it.
        turn, previous = self.turns.pop(context.context_id, (0, ""))
        self.turns[context.context_id] = (turn + 1, words)
        while len(self.turns) > MAX_CONTEXTS:
            self.turns.popitem(last=False)

        if words.lower() == "ask":
            return await task.requires_input(task.new_agent_message([new_text_part("What should I echo?")]))
        # Contract, Task states: `failed` ends the turn. Agent Kourier reads the message text to pick a category.
        if words.lower() == "fail":
            return await task.failed(task.new_agent_message([new_text_part("You asked me to fail.")]))
        text = HELP if words.lower() == "help" else f"Turn {turn + 1}. You said: {words}"
        if previous:
            text += f"\nBefore that you said: {previous}"
        await self.reply(task, text)

    async def reply(self, task: TaskUpdater, text: str) -> None:
        """Stream text as artifact appends, then complete. Contract, Replies: the answer is artifact text parts."""
        size = math.ceil(len(text) / CHUNKS)
        for start in range(0, len(text), size):
            # `append` adds to the artifact's text; the first piece, without it, starts the artifact.
            await task.add_artifact([new_text_part(text[start : start + size])], artifact_id="reply", append=start > 0)
            await asyncio.sleep(CHUNK_DELAY)
        await task.complete()  # the SDK ends the stream with the task in its final state

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        """Contract, Methods: CancelTask. The SDK stops execute(); this records that the task was canceled."""
        await TaskUpdater(event_queue, context.task_id, context.context_id).cancel()


class Handler(DefaultRequestHandler):
    """Contract, Replies: end a stream with the full Task.

    This works around the SDK: a2a-sdk 1.2.2 ends a streaming reply, and a SubscribeToTask stream, with the last status
    update, not a Task. Without this class Agent Kourier treats the stream as dropped and settles the turn with GetTask
    about a second later. Delete it if a later SDK ends its streams with the Task (test_agent.py checks that they do).
    """

    async def on_message_send_stream(self, params, context):
        async for event in self.then_task(super().on_message_send_stream(params, context), context):
            yield event

    async def on_subscribe_to_task(self, params, context):
        async for event in self.then_task(super().on_subscribe_to_task(params, context), context):
            yield event

    async def then_task(self, events, context):
        last = None
        async for last in events:
            yield last
        if isinstance(last, TaskStatusUpdateEvent):
            yield await self.on_get_task(GetTaskRequest(id=last.task_id), context)


class RequireBearer:
    """Contract, Errors: Agent Kourier sends the Binding's token as `Authorization: Bearer`; something must check it."""

    def __init__(self, app, token: bytes) -> None:
        self.app, self.expected = app, b"Bearer " + token

    async def __call__(self, scope, receive, send) -> None:
        if scope["type"] == "http":
            given = dict(scope["headers"]).get(b"authorization", b"")
            if not hmac.compare_digest(given, self.expected):
                return await PlainTextResponse("unauthorized", status_code=401)(scope, receive, send)
        await self.app(scope, receive, send)


def build_app(url: str, token_file: str = ""):
    """The ASGI app: the card at /.well-known/agent-card.json and JSON-RPC at /, so the AgentBackend's url is `url`."""
    card = build_card(url)
    handler = Handler(agent_executor=EchoExecutor(), task_store=InMemoryTaskStore(), agent_card=card)
    app = Starlette(routes=create_agent_card_routes(card) + create_jsonrpc_routes(handler, rpc_url="/"))
    if token_file:  # without a token file the agent accepts every caller: this is a demo, not a backend for real data
        with open(token_file, "rb") as f:
            token = f.read().strip()
        if not token:
            raise SystemExit(f"{token_file} is empty")
        app = RequireBearer(app, token)
    return app


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8080"))
    public_url = os.environ.get("ECHO_AGENT_URL", f"http://localhost:{port}")
    uvicorn.run(build_app(public_url, os.environ.get("ECHO_AGENT_TOKEN_FILE", "")), host="0.0.0.0", port=port)  # noqa: S104
