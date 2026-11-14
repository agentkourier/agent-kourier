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
from urllib.parse import urlparse

import uvicorn
import yaml
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

# The limits of this file. Each is a constant: only an edit of agent.py changes it, never agent.yaml or a variable.
#
# Seconds between `working` updates while a run goes on. It is an interval, not a bound: nothing happens past it. The
# contract allows 5 minutes of silence (Contract, Replies), and 120 is well inside that.
HEARTBEAT = 120
# Entries kept in memory of each kind: contexts' histories, replied messageIds and paused questions (Recent). Past it
# the oldest is forgotten: a thread whose history goes starts over; a repeated messageId whose first turn went runs
# again; an answer to a question that went does not resume the paused run, and runs as a new prompt in its thread.
MAX_KEPT = 1000
# Characters of a failed task's message. The broker reads up to 4096 characters of it, only to pick a failure category
# (Contract, Errors), so the cut only makes it shorter.
MAX_FAILED_TEXT = 1000
# Bytes of a request body: past it the agent answers 413 and reads no more (LimitBody). The broker's largest message is
# a person's turn: a trigger's prompt is at most 12002 UTF-16 units (docs/reference/limits.md), about 36 KiB of UTF-8,
# and a chat message is Slack's (40,000 characters at most, Slack's rule, not checked here), at most about 240 KiB of
# JSON even if every character were escaped. 1 MiB is four times that.
MAX_BODY = 1024 * 1024
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
    # Contract, Task states: the question is the text the thread shows, and a blank one shows nothing. The model is
    # answered here and the run goes on.
    if not question.strip():
        return "Ask a question with words in it."
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


SECRET_NAMES = ("_API_KEY", "_TOKEN", "_SECRET")


def environment_secrets() -> set[str]:
    """The values of the variables named like a secret (..._API_KEY, ..._TOKEN, ..._SECRET), for redact().

    A provider's error can quote its key, which agent.yaml never names. Only a value quoted whole is found: a key quoted
    in part, with stars between its ends, stays as it is. A value under 8 characters is left out, so that a short word
    is not cut out of every text.
    """
    values = {value.strip() for name, value in os.environ.items() if name.upper().endswith(SECRET_NAMES)}
    return {value for value in values if len(value) >= 8}


def redact(text: str, secrets) -> str:
    """Hides the secrets in text that leaves the agent: GetTask serves a failed task's message to any caller.

    Only that message is redacted. The pod's logs are not: what uvicorn, the a2a-sdk and Pydantic AI log goes out as is.
    """
    for secret in sorted(secrets, key=len, reverse=True):
        text = text.replace(secret, "[redacted]")
    return text


# The keys of Pydantic AI's agent spec (AgentSpec in the pinned version). It ignores a key it does not know, so a
# misspelt or newer key (tools:, toolsets:) would be dropped without a word; the agent refuses it instead.
SPEC_KEYS = {
    "$schema", "model", "name", "description", "instructions", "deps_schema", "output_schema", "model_settings",
    "retries", "end_strategy", "tool_timeout", "metadata", "capabilities",
}  # fmt: skip
# The provider-neutral model_settings (ModelSettings in the pinned version), less extra_body and extra_headers: the
# provider gets those as written, and extra_body can replace the request's tool list. A provider's own settings
# (openai_native_tools, bedrock_additional_model_requests_fields, ...) can add its web search or fetch, so none is
# allowed.
SETTINGS = {
    "max_tokens", "temperature", "top_p", "top_k", "timeout", "parallel_tool_calls", "tool_choice", "seed",
    "presence_penalty", "frequency_penalty", "logit_bias", "stop_sequences", "thinking", "cache", "service_tier",
}  # fmt: skip
# The providers the image has a client for (requirements.in). openai-responses: is openai: by another name.
PROVIDERS = {"anthropic", "openai", "openai-responses", "openai-chat", "openrouter", "ollama", "bedrock"}
# Models that search the web on the provider's side: OpenRouter's :online, OpenAI's search and deep research models,
# Perplexity's.
SEARCHES = re.compile(r":online|search|perplexity", re.IGNORECASE)
# The MCP keywords allowed. local: would connect to a URL of its own, with the token, instead of url.
MCP_KEYS = {"url", "allowed_tools", "native", "authorization_token", "headers", "description", "id"}


def check_model(path: str, model) -> None:
    """The spec's model must be provider:name, of a provider in PROVIDERS, and not a model that searches the web."""
    if not isinstance(model, str) or not model:
        raise SystemExit(f"{path}: model is required, as provider:name")
    provider, _, name = model.partition(":")
    if not name or provider not in PROVIDERS:
        raise SystemExit(f"{path}: model {model!r}: the provider must be one of {', '.join(sorted(PROVIDERS))}")
    if found := SEARCHES.search(name):
        raise SystemExit(f"{path}: model {model!r} ({found.group()}) makes the provider search the web itself")


def load_agent(path: str, model=None) -> tuple[Agent, set[str]]:
    """Rule: chat and alert text is untrusted, so the agent may use only the MCP tools agent.yaml lists, nothing else.

    Anything else stops the agent at start, naming the file. Pydantic AI's spec offers a shell (LocalWorkspace), web
    access and more; model_settings and the model's name can make the provider add a tool of its own; and an MCP
    `url` that is a path makes it run that script over stdio. `model` replaces the spec's, which is then not checked:
    the tests pass one. Returns the agent and the values ${NAME} expanded to.
    """
    with open(path, encoding="utf-8") as f:
        data = yaml.safe_load(f)
    if not isinstance(data, dict):
        raise SystemExit(f"{path}: must be a mapping of the agent spec's keys")
    if unknown := sorted(set(map(str, data)) - SPEC_KEYS):
        raise SystemExit(f"{path}: {', '.join(unknown)} is not a key of the agent spec")
    settings = data.get("model_settings") or {}
    if not isinstance(settings, dict):
        raise SystemExit(f"{path}: model_settings must be a mapping")
    if refused := sorted(set(map(str, settings)) - SETTINGS):
        raise SystemExit(f"{path}: model_settings {', '.join(refused)} is not allowed; these are: {sorted(SETTINGS)}")
    if model is None:
        check_model(path, data.get("model"))
    spec, secrets = AgentSpec.from_dict(data), set()
    for cap in spec.capabilities:
        args = dict(cap.kwargs)  # an MCP given as a bare string has no keyword arguments, so it fails the checks below
        if cap.name != "MCP":
            raise SystemExit(f"{path}: {cap.name} is not allowed; only MCP is")
        if refused := sorted(set(args) - MCP_KEYS):
            raise SystemExit(f"{path}: MCP {', '.join(refused)} is not allowed; these are: {sorted(MCP_KEYS)}")
        # FastMCP runs a string that ends in .py or .js and names a file as a script, over stdio, whatever else it is.
        url = args["url"] if isinstance(args.get("url"), str) else ""
        parts = urlparse(url)
        if parts.scheme not in ("http", "https") or not parts.netloc or url.lower().endswith((".py", ".js")):
            raise SystemExit(f"{path}: an MCP entry needs url: http(s)://host/..., as a keyword, and not a script")
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
    # Limits Pydantic AI sets, which agent.yaml cannot: the MCP client waits 5 seconds to connect and initialize, and
    # 5 minutes for an answer. A server that does not connect in time fails the task; a tool call with no answer in time
    # is an error the model is told of and may call again, up to agent.yaml's `retries`, and then the task fails.
    # agent.yaml's `tool_timeout` does not reach MCP calls: Pydantic AI puts it on its own function tools, and the only
    # one here is ask_user, which never waits.
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
        # over. A thread's own history is not trimmed: a long thread grows until the model refuses it as longer than its
        # context window. That turn fails, and a failed turn saves nothing, so every later turn of the thread fails the
        # same way; a new thread starts empty.
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
            # Pydantic AI allows 50 model responses in one run (UsageLimits' request_limit), counted again from 0 on
            # each turn and on the run an answer resumes. Past it the task fails with UsageLimitExceeded. agent.yaml
            # cannot change it.
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
                # Contract, Task states: a question is `input-required` with one text part. Only ask_user defers. One
                # question at a time: when the model asks several in one response, only the first is posted, and the
                # answer to it resumes the run with the others answered "Ask one question at a time." (execute()).
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
            text = redact(f"{type(e).__name__}: {e}", self.secrets)[:MAX_FAILED_TEXT]
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


class LimitBody:
    """Answers 413 to a request body of more than MAX_BODY bytes, and the app never sees it.

    A Content-Length over the cap is answered before the body is read. Otherwise the bytes are counted as they arrive,
    so a chunked body, which has no Content-Length, is held to the cap too. The body is handed on whole once it is read:
    the JSON-RPC route reads a whole body anyway.
    """

    def __init__(self, app) -> None:
        self.app = app

    async def __call__(self, scope, receive, send) -> None:
        if scope["type"] != "http":
            return await self.app(scope, receive, send)
        too_large = PlainTextResponse("request body too large", status_code=413)
        length = dict(scope["headers"]).get(b"content-length", b"")
        if length.isdigit() and int(length) > MAX_BODY:
            return await too_large(scope, receive, send)
        chunks, size, more = [], 0, True
        while more:
            message = await receive()
            if message["type"] != "http.request":  # the caller went away
                return None
            chunks.append(message.get("body", b""))
            size, more = size + len(chunks[-1]), message.get("more_body", False)
            if size > MAX_BODY:
                return await too_large(scope, receive, send)
        body = {"type": "http.request", "body": b"".join(chunks), "more_body": False}

        async def replay():  # the body first, then whatever comes next: a disconnect, which a stream listens for
            nonlocal body
            message, body = body, None
            return message or await receive()

        await self.app(scope, replay, send)


def build_app(url: str, spec_path: str = "agent.yaml", token_file: str = "", model=None, anonymous: str = ""):
    """The ASGI app: the card at /.well-known/agent-card.json and JSON-RPC at /, so the AgentBackend's url is `url`."""
    # Rule: the agent fails closed. Without the Binding's token to check, any caller that reaches it could read and add
    # to every conversation and spend the model key, so it starts without one only when told to, with exactly "1": a
    # "0" or a "false" is never taken for yes.
    if not token_file and anonymous != "1":
        raise SystemExit(
            "PROMPT_AGENT_TOKEN_FILE is not set: set it to the file of the token the Binding sends, or set"
            " PROMPT_AGENT_ALLOW_ANONYMOUS=1 to accept every caller"
        )
    agent, secrets = load_agent(spec_path, model)
    card = build_card(agent, url)
    # Tasks stay in memory until a restart, unbounded like the echo agent's: GetTask must find them (-32001 reads as the
    # session gone), and so must ListTasks (a turn it cannot find is sent again). Past the pod's memory limit (512Mi in
    # manifests/deployment.yaml) the pod is killed: every thread starts over, and the broker reads the -32001 of a task
    # it follows as the session gone.
    executor = PromptExecutor(agent, secrets | environment_secrets())
    handler = DefaultRequestHandler(agent_executor=executor, task_store=InMemoryTaskStore(), agent_card=card)
    app = LimitBody(Starlette(routes=create_agent_card_routes(card) + create_jsonrpc_routes(handler, rpc_url="/")))
    if token_file:
        with open(token_file, "rb") as f:  # a missing or empty file stops the agent at start
            if not f.read().strip():
                raise SystemExit(f"{token_file} is empty")
        app = RequireBearer(app, token_file)  # outermost: a caller without the token gets 401, and its body is not read
    return app


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8080"))
    app = build_app(
        os.environ.get("PROMPT_AGENT_URL", f"http://localhost:{port}"),
        os.environ.get("PROMPT_AGENT_SPEC", "agent.yaml"),
        os.environ.get("PROMPT_AGENT_TOKEN_FILE", ""),
        anonymous=os.environ.get("PROMPT_AGENT_ALLOW_ANONYMOUS", ""),
    )
    uvicorn.run(app, host="0.0.0.0", port=port)  # noqa: S104 (all interfaces, so the Service can reach the pod)
