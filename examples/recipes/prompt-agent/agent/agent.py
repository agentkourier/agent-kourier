"""A prompt agent for Agent Kourier: the Pydantic AI agent spec in agent.yaml, behind an a2a-sdk server.

Read it top to bottom. Each block names the row of docs/reference/agent-contract.md it satisfies, or the rule it
enforces.
The a2a-sdk serves the protocol (JSON-RPC, streaming, GetTask, ListTasks, SubscribeToTask, CancelTask); Pydantic AI runs
the model and the MCP tools.
"""

import asyncio
import contextlib
import hmac
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
from a2a.types.a2a_pb2 import AgentCapabilities, AgentCard, AgentInterface
from pydantic_ai import Agent, CallDeferred, DeferredToolRequests, DeferredToolResults, RunCancelled
from pydantic_ai.agent.spec import AgentSpec
from pydantic_ai.messages import (
    ModelMessagesTypeAdapter,
    PartDeltaEvent,
    PartStartEvent,
    TextPart,
    TextPartDelta,
    repair_messages,
)
from pydantic_ai.run import AgentRunResultEvent
from starlette.applications import Starlette
from starlette.responses import PlainTextResponse

HEARTBEAT = 120  # seconds between `working` updates during a run; the contract allows 5 minutes of silence
MAX_KEPT = 1000  # contexts, replied messageIds and paused questions kept in memory; the oldest is forgotten past this
ENV = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")
CONTROL = re.compile(r"[\x00-\x1f\x7f]")


class Recent(OrderedDict):
    """A dict that forgets its oldest entry past MAX_KEPT, so a long-lived pod does not grow without end."""

    def __setitem__(self, key, value) -> None:
        super().__setitem__(key, value)
        self.move_to_end(key)
        while len(self) > MAX_KEPT:
            self.popitem(last=False)


def ask_user(question: str) -> str:
    """Ask the person in the chat thread one question; their answer is the result."""
    # Ends the run with DeferredToolRequests, which run() posts as an `input-required` question.
    raise CallDeferred


def expand(value: str, path: str, secrets: set[str]) -> str:
    """Replaces ${NAME} with the environment variable NAME, so a token is never written in agent.yaml.

    The value is stripped (a Secret made from a file often ends in a newline, which is not a legal header value) and
    must then be non-empty and free of control characters. Each value goes into `secrets`, for redact() to remove.
    """

    def environment(match: re.Match) -> str:
        name = match.group(1)
        if name not in os.environ:
            raise SystemExit(f"{path}: the environment variable {name} is not set")
        text = os.environ[name].strip()
        if not text or CONTROL.search(text):  # never quote the value: it is a secret
            raise SystemExit(f"{path}: the environment variable {name} is empty or holds a control character")
        secrets.add(text)
        return text

    return ENV.sub(environment, value)


def redact(text: str, secrets) -> str:
    """Hides the expanded values in text that leaves the agent: GetTask serves a failed task's message to any caller."""
    for secret in sorted(secrets, key=len, reverse=True):
        text = text.replace(secret, "[redacted]")
    return text


def load_agent(path: str, model=None) -> tuple[Agent, set[str]]:
    """Rule: chat and alert text is untrusted, so the agent may use only the MCP tools agent.yaml lists, nothing else.

    Anything else stops the agent at start, naming the file. Pydantic AI's spec offers a shell (LocalWorkspace), web
    access and more, and an MCP `url` that is a path makes it run that script over stdio. `model` replaces the spec's.
    Returns the agent and the values ${NAME} expanded to.
    """
    spec, secrets = AgentSpec.from_file(path), set()
    for cap in spec.capabilities:
        args = dict(cap.kwargs)  # an MCP given as a bare string has no keyword arguments, so it fails the checks below
        if cap.name != "MCP":
            raise SystemExit(f"{path}: {cap.name} is not allowed; only MCP is")
        if not str(args.get("url", "")).startswith(("http://", "https://")):
            raise SystemExit(
                f"{path}: an MCP entry needs url: http(s)://..., as a keyword, not a path, a command or a string"
            )
        # Pydantic AI filters tools only for a list: null, or no list at all, would offer every tool the server has.
        tools = args.get("allowed_tools")
        if not (isinstance(tools, list) and tools and all(isinstance(t, str) for t in tools)):
            raise SystemExit(f"{path}: an MCP entry must list allowed_tools, as names: the tools the agent may call")
        # Anything but false (a mapping, null, true) makes the model provider call the server itself, with the token.
        if args.get("native", False) is not False:
            raise SystemExit(f"{path}: native MCP is not allowed: the model provider would call the server itself")
        # A token belongs in these two places, and only here is ${NAME} expanded: an expanded prompt would send the
        # token to the model.
        if "authorization_token" in args:
            if not isinstance(args["authorization_token"], str):
                raise SystemExit(f"{path}: authorization_token must be a string")
            args["authorization_token"] = expand(args["authorization_token"], path, secrets)
        headers = args.get("headers")
        if headers is not None:
            if not (isinstance(headers, dict) and all(isinstance(v, str) for v in headers.values())):
                raise SystemExit(f"{path}: headers must be a mapping of names to string values")
            args["headers"] = {name: expand(value, path, secrets) for name, value in headers.items()}
        cap.arguments = args
    agent = Agent.from_spec(spec, model=model, tools=[ask_user], output_type=[str, DeferredToolRequests])
    return agent, secrets


def build_card(agent: Agent, url: str) -> AgentCard:
    """Contract, Discovery: streaming on, one JSON-RPC interface, no required extension. Name and text: agent.yaml's."""
    return AgentCard(
        name=agent.name or "prompt-agent",
        description=str(agent.description or ""),
        version="1.0.0",
        supported_interfaces=[AgentInterface(url=url, protocol_binding="JSONRPC", protocol_version="1.0")],
        capabilities=AgentCapabilities(streaming=True),  # Agent Kourier never uses the non-streaming send
        default_input_modes=["text/plain"],
        default_output_modes=["text/plain"],
    )


def words_of(context: RequestContext) -> str:
    """The person's words, without the prefix line Agent Kourier adds and any <@user> mention Slack leaves in."""
    text = context.get_user_input()
    if "agentkourier.dev/actor" in context.message.metadata:  # Contract, What Agent Kourier sends: a person's turn
        text = text.split("\n", 1)[-1]
    return re.sub(r"<@[^>]+>", "", text).strip()


def text_of(event, started: bool) -> str:
    """The text a model event adds to the reply; a text part that follows a tool call starts a new paragraph."""
    if isinstance(event, PartStartEvent) and isinstance(event.part, TextPart):
        return ("\n\n" if started else "") + event.part.content
    if isinstance(event, PartDeltaEvent) and isinstance(event.delta, TextPartDelta):
        return event.delta.content_delta
    return ""


@contextlib.asynccontextmanager
async def heartbeat(task: TaskUpdater):
    """Contract, Replies: an event at least every 5 minutes, even while one tool call runs: `working`, repeated."""

    async def beat() -> None:
        while True:
            await asyncio.sleep(HEARTBEAT)
            await task.start_work()

    beating = asyncio.create_task(beat())
    try:
        yield
    finally:
        beating.cancel()


class PromptExecutor(AgentExecutor):
    """One A2A task per turn: execute() runs it and cancel() stops it."""

    def __init__(self, agent: Agent, secrets: set[str]) -> None:
        self.agent, self.secrets = agent, secrets
        # All of this is in memory and bounded (MAX_KEPT): after a restart, or enough other threads, a thread starts
        # over.
        self.history = Recent()  # contextId -> the run's messages as JSON
        self.paused = Recent()  # taskId -> the ids of the ask_user calls an answer resolves
        self.turns = Recent()  # messageId -> a future of (contextId, reply) once that turn completed, else None

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        # Contract, Replies: every event names the task and its contextId, and the SDK needs the Task to come first.
        if context.current_task is None:
            await event_queue.enqueue_event(new_task_from_user_message(context.message))
        task = TaskUpdater(event_queue, context.task_id, context.context_id)
        await task.start_work()

        ctx, message_id, words = context.context_id, context.message.message_id, words_of(context)
        history = ModelMessagesTypeAdapter.validate_json(self.history.get(ctx, b"[]"))
        outcome = asyncio.Future()
        if calls := self.paused.pop(context.task_id, None):
            # Contract, Task states: Agent Kourier sends the person's answer to the paused task, and it resumes the run.
            answers = {call: words if i == 0 else "Ask one question at a time." for i, call in enumerate(calls)}
            prompt, results = None, DeferredToolResults(calls=answers)
        else:
            if await self.repeat(message_id, ctx, task):
                return
            self.turns[message_id] = outcome
            # A question the broker cancelled leaves a tool call unanswered, and Pydantic AI refuses a new prompt
            # after it.
            prompt, results, history = words, None, repair_messages(history)
        reply = None
        try:
            reply = await self.run(task, ctx, prompt, results, history)
        finally:
            outcome.set_result(None if reply is None else (ctx, reply))  # only a completed turn is a reply to repeat

    async def repeat(self, message_id: str, ctx: str, task: TaskUpdater) -> bool:
        """Contract, Replies: a repeated messageId is the same turn. If its first run completes, answer with its reply.

        A first turn is resent with no contextId after an unknown outcome, so the repeat arrives in a new context, which
        inherits the conversation. A repeat that arrives while the first run goes on waits for it, with a heartbeat of
        its own, and the shield keeps its own cancel from cancelling the first run's future. A failed run runs again.
        """
        first = self.turns.get(message_id)
        if first is None:
            return False
        async with heartbeat(task):
            done = await asyncio.shield(first)
        if not done:
            return False
        first_ctx, reply = done
        self.history[ctx] = self.history.get(first_ctx, b"[]")
        if reply:
            await task.add_artifact([new_text_part(reply)], artifact_id="reply")
        await task.complete()
        return True

    async def run(self, task: TaskUpdater, ctx: str, prompt, results, history) -> str | None:
        """Runs the agent into `task`: appends text, then completes, fails or asks. Returns the reply if completed."""
        started, text = False, ""
        try:
            async with (
                heartbeat(task),
                self.agent.run_stream_events(prompt, message_history=history, deferred_tool_results=results) as events,
            ):
                async for event in events:
                    if piece := text_of(event, started):
                        # Contract, Replies: the answer is artifact text parts, each an append to the one artifact.
                        # A short reply is dozens of appends; Agent Kourier throttles its writes to the chat (2 seconds
                        # by default, internal/render/live), so nothing is batched here.
                        await task.add_artifact([new_text_part(piece)], artifact_id="reply", append=started)
                        started, text = True, text + piece
                    elif isinstance(event, AgentRunResultEvent):
                        result = event.result
            self.history[ctx] = ModelMessagesTypeAdapter.dump_json(result.all_messages())
            if isinstance(result.output, DeferredToolRequests):
                # Contract, Task states: a question is `input-required` with one text part. Only ask_user defers.
                calls = [call for call in result.output.calls if call.tool_name == "ask_user"]
                self.paused[task.task_id] = [call.tool_call_id for call in calls]
                await task.requires_input(task.new_agent_message([new_text_part(calls[0].args_as_dict()["question"])]))
                return None
            await task.complete()
            return text
        except asyncio.CancelledError as e:
            # Contract, Methods: CancelTask. The SDK cancels execute() after cancel() below. Keep the run's messages so
            # far, so the next turn builds on them, and let the cancellation go on.
            if state := RunCancelled.from_cancellation(e):
                self.history[ctx] = ModelMessagesTypeAdapter.dump_json(state.all_messages())
            raise
        except Exception as e:  # noqa: BLE001 (any failure ends the task failed, with a message to categorise)
            # Contract, Task states: Agent Kourier reads this text, and only this, to pick a failure category. An MCP
            # server's 401 arrives as "MCPError: Server returned an error response", with no status to map to
            # "authentication": the MCP client drops it (mcp/client/streamable_http.py), as for other error statuses.
            # An exception can quote the header it was sent with, and GetTask serves this text to any caller.
            text = redact(f"{type(e).__name__}: {e}", self.secrets)[:1000]
            await task.failed(task.new_agent_message([new_text_part(text)]))
            return None

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        """Contract, Methods: CancelTask. Drops a question that timed out; the SDK cancels execute() and so the run."""
        self.paused.pop(context.task_id, None)
        await TaskUpdater(event_queue, context.task_id, context.context_id).cancel()


class RequireBearer:
    """Contract, Errors: Agent Kourier sends the Binding's token as `Authorization: Bearer`; something must check it.

    The file is read on every request, so a rotated token works without a restart. A mounted Secret reaches the pod
    some time after Agent Kourier's watch sees the change, so a few 401s are possible meanwhile.
    """

    def __init__(self, app, token_file: str) -> None:
        self.app, self.token_file = app, token_file

    def allows(self, given: bytes) -> bool:
        try:  # a blocking read of a few bytes; a busy server would cache it by the file's mtime
            with open(self.token_file, "rb") as f:
                token = f.read().strip()
        except OSError:
            return False
        return bool(token) and hmac.compare_digest(given, b"Bearer " + token)

    async def __call__(self, scope, receive, send) -> None:
        if scope["type"] != "lifespan" and not self.allows(dict(scope["headers"]).get(b"authorization", b"")):
            if scope["type"] == "http":
                return await PlainTextResponse("unauthorized", status_code=401)(scope, receive, send)
            return await send({"type": "websocket.close"})  # fail closed for anything that is not HTTP
        await self.app(scope, receive, send)


def build_app(url: str, spec_path: str = "agent.yaml", token_file: str = "", model=None):
    """The ASGI app: the card at /.well-known/agent-card.json and JSON-RPC at /, so the AgentBackend's url is `url`."""
    agent, secrets = load_agent(spec_path, model)
    card = build_card(agent, url)
    # Tasks stay in memory until a restart, unbounded like the echo agent's: GetTask must find them (-32001 reads as the
    # session gone), and so must ListTasks (a turn it cannot find is sent again).
    handler = DefaultRequestHandler(
        agent_executor=PromptExecutor(agent, secrets), task_store=InMemoryTaskStore(), agent_card=card
    )
    app = Starlette(routes=create_agent_card_routes(card) + create_jsonrpc_routes(handler, rpc_url="/"))
    if token_file:  # without a token file the agent accepts every caller, and spends your model key for them
        with open(token_file, "rb") as f:  # a missing or empty file stops the agent at start
            if not f.read().strip():
                raise SystemExit(f"{token_file} is empty")
        app = RequireBearer(app, token_file)
    return app


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8080"))
    app = build_app(
        os.environ.get("PROMPT_AGENT_URL", f"http://localhost:{port}"),
        os.environ.get("PROMPT_AGENT_SPEC", "agent.yaml"),
        os.environ.get("PROMPT_AGENT_TOKEN_FILE", ""),
    )
    uvicorn.run(app, host="0.0.0.0", port=port)  # noqa: S104 (all interfaces, so the Service can reach the pod)
