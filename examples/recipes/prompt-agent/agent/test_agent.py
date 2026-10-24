"""Tests for agent.py, driven through the a2a-sdk's own client over an in-process ASGI transport: no port, no network.

The model is scripted and the MCP server is the real one in mcp_server.py (see harness.py). Each test names the row of
docs/reference/agent-contract.md or the rule of agent.py it proves.
"""

import asyncio
import json
import time
from pathlib import Path

import pytest
import yaml
from a2a.types.a2a_pb2 import CancelTaskRequest, ListTasksRequest
from pydantic_ai.messages import RetryPromptPart, ToolReturnPart, UserPromptPart
from pydantic_ai.models.function import DeltaToolCall, FunctionModel

import agent
from harness import (
    CANCELED,
    DONE,
    FAILED,
    HERE,
    INPUT,
    TOKEN,
    URL,
    artifact_text,
    asgi,
    running_task,
    say,
    serve,
    spec_file,
    until,
    working,
)

ACTOR = "agentkourier.dev/actor"
CALLS = {  # what the prompt makes the scripted model call
    "runbook": ("runbook", {"alert": "DiskFull"}),
    "slow": ("slow_check", {"seconds": 30}),
    "ask": ("ask_user", {"question": "Which cluster?"}),
    "inject": ("restart_cluster", {}),
}


def scripted(seen: list | None = None, release: asyncio.Event | None = None):
    """A model that streams text in pieces, or calls the tool CALLS names for the prompt.

    `seen` gets one entry per model call: the tools the model was offered. A prompt of `hold` waits for `release`.
    """

    async def stream(messages, info):
        if seen is not None:
            seen.append(sorted(t.name for t in info.function_tools))
        last = messages[-1].parts[-1]
        if isinstance(last, ToolReturnPart):
            yield f"Tool said: {last.content}"
        elif isinstance(last, RetryPromptPart):
            yield f"Refused: {last.model_response()}"
        elif last.content == "boom":
            raise RuntimeError("model exploded")
        elif last.content == "leak":
            raise RuntimeError(f"rejected Bearer {TOKEN}")
        elif last.content == "hold":
            await release.wait()
            yield "held"
        elif last.content in CALLS:
            name, args = CALLS[last.content]
            yield {0: DeltaToolCall(name=name, json_args=json.dumps(args))}
        else:
            turn = sum(isinstance(p, UserPromptPart) for m in messages for p in m.parts)
            for piece in ["Turn ", str(turn), ". You said: ", last.content]:
                yield piece

    return FunctionModel(stream_function=stream)


@pytest.fixture
def spec(tmp_path, server):
    return spec_file(tmp_path, server.url)


@pytest.mark.anyio
async def test_card_is_served_with_streaming_a_jsonrpc_interface_and_the_specs_name(spec):
    """Contract, Discovery and transport: card at the well-known path, streaming on, JSON-RPC, no required extension."""
    async with asgi(spec, scripted()) as http:
        card = (await http.get("/.well-known/agent-card.json")).json()
    assert (card["name"], card["description"]) == ("ops-helper", "Answers questions about alerts.")
    assert card["capabilities"]["streaming"] is True
    assert [(i["protocolBinding"], i["protocolVersion"]) for i in card["supportedInterfaces"]] == [("JSONRPC", "1.0")]
    assert not [e for e in card["capabilities"].get("extensions", []) if e.get("required")]


@pytest.mark.anyio
async def test_text_streams_as_artifact_appends_and_the_task_completes(spec):
    """Contract, Replies: events name the task and its contextId, the answer is artifact text, the end is terminal."""
    async with serve(spec, scripted()) as client:
        events, task = await say(client, "hello")
    assert events[0].HasField("task"), "the first event names the task"
    pieces = [e.artifact_update for e in events if e.HasField("artifact_update")]
    assert len(pieces) == 4 and [p.append for p in pieces] == [False, True, True, True]
    assert all((p.task_id, p.context_id) == (task.id, task.context_id) and task.context_id for p in pieces)
    assert task.status.state == DONE and artifact_text(task) == "Turn 1. You said: hello"
    assert "".join(part.text for p in pieces for part in p.artifact.parts) == artifact_text(task)
    # a2a-sdk 1.2.2 ends the stream with a status update, which the broker settles with one GetTask: no patch needed
    assert events[-1].HasField("status_update") and events[-1].status_update.status.state == DONE


@pytest.mark.anyio
async def test_text_after_a_tool_call_is_a_new_paragraph_of_the_same_artifact(spec):
    """Contract, Replies: one artifact, appended to; the text of a later model response starts after a blank line."""

    async def stream(messages, info):
        if isinstance(messages[-1].parts[-1], ToolReturnPart):
            yield "Done."
        else:
            yield "Checking. "
            yield {0: DeltaToolCall(name="runbook", json_args=json.dumps({"alert": "x"}))}

    async with serve(spec, FunctionModel(stream_function=stream)) as client:
        _, task = await say(client, "go")
    assert artifact_text(task) == "Checking. \n\nDone." and len(task.artifacts) == 1


@pytest.mark.anyio
async def test_a_second_turn_in_the_context_sees_the_first_and_another_context_does_not(spec):
    """Contract, Replies: a thread keeps its contextId, and what was said in it is remembered."""
    async with serve(spec, scripted()) as client:
        _, first = await say(client, "one")
        _, second = await say(client, "two", context_id=first.context_id)
        _, other = await say(client, "elsewhere")
    assert second.context_id == first.context_id and second.id != first.id
    assert artifact_text(second) == "Turn 2. You said: two"
    assert artifact_text(other) == "Turn 1. You said: elsewhere"


@pytest.mark.anyio
async def test_the_actor_prefix_and_a_bot_mention_are_not_part_of_the_prompt(spec):
    """Contract, What Agent Kourier sends: a person's turn has a prefix line and the actor in its metadata."""
    actor = {ACTOR: {"id": "U123", "platform": "slack", "name": "Ada"}}
    async with serve(spec, scripted()) as client:
        _, task = await say(client, "[Slack: Ada (U123)]\n<@UBOT> help me", metadata=actor)
        _, plain = await say(client, "no actor here\nsecond line")  # no metadata: nothing is stripped
    assert artifact_text(task) == "Turn 1. You said: help me"
    assert artifact_text(plain) == "Turn 1. You said: no actor here\nsecond line"


@pytest.mark.anyio
async def test_an_mcp_tool_is_called_with_the_expanded_token_and_only_allowed_tools_are_offered(spec, server):
    """agent.yaml's ${OPS_MCP_TOKEN} reaches the server as the bearer; restart_cluster is on the server, not offered."""
    seen = []
    async with serve(spec, scripted(seen)) as client:
        _, task = await say(client, "runbook")
    assert task.status.state == DONE and "Runbook for DiskFull" in artifact_text(task)
    assert seen[0] == ["ask_user", "runbook", "slow_check"]
    assert [(c["tool"], c["phase"]) for c in server.calls()] == [("runbook", "call")]


@pytest.mark.anyio
async def test_headers_expand_the_variable_too(tmp_path, server):
    spec = spec_file(
        tmp_path, server.url, authorization_token=None, headers={"Authorization": "Bearer ${OPS_MCP_TOKEN}"}
    )
    async with serve(spec, scripted()) as client:
        _, task = await say(client, "runbook")
    assert task.status.state == DONE and [c["tool"] for c in server.calls()] == ["runbook"]


def test_an_unset_variable_stops_the_agent_at_start_and_names_it(spec, monkeypatch):
    monkeypatch.delenv("OPS_MCP_TOKEN")
    with pytest.raises(SystemExit, match="OPS_MCP_TOKEN"):
        agent.load_agent(spec, scripted())


@pytest.mark.anyio
async def test_the_variable_is_not_expanded_in_the_prompt_so_a_prompt_cannot_pull_in_a_secret(tmp_path, server):
    """Only MCP authorization_token and headers are expanded: an expanded prompt would send the secret to the model."""
    spec = spec_file(tmp_path, server.url)
    text = Path(spec).read_text().replace("You help on-call engineers.", "You help. ${OPS_MCP_TOKEN}")
    Path(spec).write_text(text)
    instructions = []

    async def stream(messages, info):
        instructions.append(messages[-1].instructions)
        yield "ok"

    async with serve(spec, FunctionModel(stream_function=stream)) as client:
        await say(client, "hi")
    assert "${OPS_MCP_TOKEN}" in instructions[0] and TOKEN not in instructions[0]


def test_the_example_agent_yaml_passes_the_policy(monkeypatch):
    monkeypatch.setenv("RUNBOOKS_MCP_TOKEN", TOKEN)
    loaded, secrets = agent.load_agent(str(HERE / "agent.yaml"), scripted())
    assert loaded.name and secrets == {TOKEN}


@pytest.mark.anyio
async def test_a_token_with_a_trailing_newline_works(spec, server, monkeypatch):
    """A Secret made from a file often ends in a newline, which is not a legal header value."""
    monkeypatch.setenv("OPS_MCP_TOKEN", f"  {TOKEN}\n")
    async with serve(spec, scripted()) as client:
        _, task = await say(client, "runbook")
    assert task.status.state == DONE and [c["tool"] for c in server.calls()] == ["runbook"]


@pytest.mark.parametrize("value", ["", " \n", "fake\nmore", "fake\x07", "fake\tmore"])
def test_a_variable_that_is_empty_or_holds_a_control_character_stops_the_agent_without_showing_it(
    spec, monkeypatch, value
):
    monkeypatch.setenv("OPS_MCP_TOKEN", value)
    with pytest.raises(SystemExit, match="OPS_MCP_TOKEN") as stopped:
        agent.load_agent(spec, scripted())
    assert "fake" not in str(stopped.value)


@pytest.mark.anyio
async def test_the_token_is_redacted_from_the_failed_message_that_GetTask_serves(spec):
    """An exception can quote the header it was sent with; the task store serves the status message to any caller."""
    async with serve(spec, scripted()) as client:
        _, task = await say(client, "leak")
    text = task.status.message.parts[0].text
    assert task.status.state == FAILED and TOKEN not in text and text == "RuntimeError: rejected Bearer [redacted]"


REFUSALS = {  # an agent.yaml `capabilities:` list, and what the refusal must name
    "a capability that is not MCP": ("- LocalWorkspace: {working_dir: /}", "LocalWorkspace"),
    "a capability with no arguments": ("- WebFetch", "WebFetch"),
    "MCP without allowed_tools": ("- MCP: {url: 'http://x/mcp'}", "allowed_tools"),
    "MCP given its url alone": ("- MCP: 'http://x/mcp'", "url"),
    "MCP with allowed_tools null": ("- MCP: {url: 'http://x/mcp', allowed_tools: null}", "allowed_tools"),
    "MCP with a blank allowed_tools": ("- MCP:\n      url: 'http://x/mcp'\n      allowed_tools:", "allowed_tools"),
    "MCP with allowed_tools empty": ("- MCP: {url: 'http://x/mcp', allowed_tools: []}", "allowed_tools"),
    "MCP with allowed_tools a string": ("- MCP: {url: 'http://x/mcp', allowed_tools: runbook}", "allowed_tools"),
    "MCP with a tool name that is not a string": ("- MCP: {url: 'http://x/mcp', allowed_tools: [1]}", "allowed_tools"),
    "MCP with native on": ("- MCP: {url: 'http://x/mcp', allowed_tools: [a], native: true}", "native"),
    "MCP with native an empty mapping": ("- MCP: {url: 'http://x/mcp', allowed_tools: [a], native: {}}", "native"),
    "MCP with native null": ("- MCP: {url: 'http://x/mcp', allowed_tools: [a], native: null}", "native"),
    "MCP with a null authorization_token": (
        "- MCP: {url: 'http://x/mcp', allowed_tools: [a], authorization_token: null}",
        "authorization_token",
    ),
    "MCP with headers that are not a mapping": (
        "- MCP: {url: 'http://x/mcp', allowed_tools: [a], headers: [a]}",
        "headers",
    ),
    "MCP with a header value that is not a string": (
        "- MCP: {url: 'http://x/mcp', allowed_tools: [a], headers: {X-Id: 5}}",
        "headers",
    ),
    "MCP with a script path as its url": ("- MCP: {url: mcp_server.py, allowed_tools: [runbook]}", "url"),
    "MCP with a command as its url": ("- MCP: {url: 'python mcp_server.py', allowed_tools: [a]}", "url"),
    "MCP with a file url": ("- MCP: {url: 'file:///etc/passwd', allowed_tools: [a]}", "url"),
    "an allowed MCP and one that is not": (
        "- MCP: {url: 'http://x/mcp', allowed_tools: [a]}\n  - WebSearch",
        "WebSearch",
    ),
}


@pytest.mark.parametrize(("capabilities", "names"), REFUSALS.values(), ids=REFUSALS.keys())
def test_the_agent_refuses_to_start_on_anything_but_mcp_over_http_with_allowed_tools_and_not_native(
    tmp_path, capabilities, names
):
    """Chat and alert text is untrusted: no shell, files or web, and a url that is a path would run a script."""
    path = tmp_path / "agent.yaml"
    path.write_text(f"model: test\ncapabilities:\n  {capabilities}\n")
    with pytest.raises(SystemExit, match=f"{path}.*{names}"):
        agent.load_agent(str(path), scripted())


@pytest.mark.anyio
async def test_a_call_to_a_tool_that_is_not_allowed_never_reaches_the_server(spec, server):
    """The filtered toolset, not the model, holds the line: Pydantic AI answers an unknown tool name itself."""
    async with serve(spec, scripted()) as client:
        _, task = await say(client, "inject")
    assert task.status.state == DONE and "Unknown tool name: 'restart_cluster'" in artifact_text(task)
    assert server.calls() == []


@pytest.mark.anyio
async def test_cancel_during_a_slow_mcp_call_ends_canceled_within_a_second_and_the_server_sees_it(spec, server):
    """Contract, Methods: CancelTask. The 30-second tool call does not hold the cancel."""
    async with serve(spec, scripted()) as client:
        stream = asyncio.create_task(say(client, "slow"))
        await until(lambda: any(c["phase"] == "start" for c in server.calls()), "the tool call to start")
        started = time.monotonic()
        canceled = await client.cancel_task(CancelTaskRequest(id=(await running_task(client)).id))
        events, task = await stream
        took = time.monotonic() - started
        await until(lambda: any(c["phase"] == "cancelled" for c in server.calls()), "the server to see the cancel")
    assert canceled.status.state == CANCELED and task.status.state == CANCELED
    assert took < 1, "the stream ended when the cancel did"
    assert [c["phase"] for c in server.calls() if c["tool"] == "slow_check"] == ["start", "cancelled"]


@pytest.mark.anyio
async def test_the_next_turn_after_a_cancel_runs_and_the_work_before_the_cancel_is_remembered(spec, server):
    """Cancelling saves the run's messages: the next turn is built on them, and is neither wedged nor a first."""
    async with serve(spec, scripted()) as client:
        _, done = await say(client, "one")
        stream = asyncio.create_task(say(client, "slow", context_id=done.context_id))
        await until(lambda: any(c["phase"] == "start" for c in server.calls()), "the tool call to start")
        await client.cancel_task(CancelTaskRequest(id=(await running_task(client)).id))
        _, canceled = await stream
        _, after = await say(client, "next", context_id=done.context_id)
    assert canceled.status.state == CANCELED
    assert after.status.state == DONE and artifact_text(after) == "Turn 3. You said: next"


@pytest.mark.anyio
async def test_ask_user_pauses_with_a_text_question_and_the_answer_completes_the_same_task(spec):
    """Contract, Task states: a question is input-required with text parts only; the answer is sent to the same task."""
    async with serve(spec, scripted()) as client:
        _, question = await say(client, "ask")
        assert question.status.state == INPUT
        parts = question.status.message.parts
        assert [p.text for p in parts] == ["Which cluster?"] and all(p.WhichOneof("content") == "text" for p in parts)
        assert question.status.message.message_id
        _, answer = await say(client, "prod-east", context_id=question.context_id, task_id=question.id)
        _, later = await say(client, "and now", context_id=question.context_id)
    assert (answer.id, answer.context_id, answer.status.state) == (question.id, question.context_id, DONE)
    assert artifact_text(answer) == "Tool said: prod-east"
    assert artifact_text(later) == "Turn 2. You said: and now", "the answer is a tool result, not a turn"


@pytest.mark.anyio
async def test_a_question_cancelled_while_paused_leaves_the_context_usable(spec):
    """The broker cancels a question that times out; the saved history ends in an unanswered tool call."""
    async with serve(spec, scripted()) as client:
        _, question = await say(client, "ask")
        canceled = await client.cancel_task(CancelTaskRequest(id=question.id))
        _, after = await say(client, "next", context_id=question.context_id)
    assert canceled.status.state == CANCELED
    assert after.status.state == DONE and artifact_text(after).endswith("You said: next")


@pytest.mark.anyio
async def test_a_model_error_ends_the_task_failed_with_a_message(spec):
    """Contract, Task states: failed ends the turn; Agent Kourier reads the status message's text for a category."""
    async with serve(spec, scripted()) as client:
        _, task = await say(client, "boom")
    assert task.status.state == FAILED and task.status.message.parts[0].text == "RuntimeError: model exploded"


@pytest.mark.anyio
async def test_a_wrong_mcp_token_fails_the_task_with_a_message(spec, server, monkeypatch):
    """The server's 401 ends the task failed. The MCP client turns any HTTP error status into the same
    `MCPError: Server returned an error response` (mcp/client/streamable_http.py), so the message cannot say
    "authentication": the status is not exposed to the agent, and Agent Kourier picks no failure category.
    """
    monkeypatch.setenv("OPS_MCP_TOKEN", "wrong-fake-token")
    async with serve(spec, scripted()) as client:
        _, task = await say(client, "runbook")
    assert task.status.state == FAILED and task.status.message.parts[0].text.startswith("MCPError")
    assert {c["phase"] for c in server.calls()} == {"unauthorized"}


@pytest.mark.anyio
async def test_a_working_status_goes_out_every_heartbeat_while_a_tool_call_runs(spec, server, clock):
    """Contract, Replies: an event at least every 5 minutes, including during a long tool call. The clock is a fake.

    The heartbeat asks for its next sleep right after it publishes, so the clock's requests count the updates sent.
    """
    async with serve(spec, scripted()) as client:
        stream = asyncio.create_task(say(client, "slow"))
        await until(lambda: any(c["phase"] == "start" for c in server.calls()), "the tool call to start")
        for sent in (2, 3):
            clock.tick()
            await until(lambda sent=sent: len(clock.requested) == sent, "a heartbeat")
        await client.cancel_task(CancelTaskRequest(id=(await running_task(client)).id))
        events, _ = await stream
    assert 0 < agent.HEARTBEAT < 300 and clock.requested == [agent.HEARTBEAT] * 3
    assert len(working(events)) == 3, "one `working` when the turn began, and a heartbeat for each tick"


@pytest.mark.anyio
async def test_no_heartbeat_goes_out_once_the_turn_has_ended(spec, clock):
    async with serve(spec, scripted()) as client:
        await say(client, "hello")
        await asyncio.sleep(0.05)
    assert clock.requested == [agent.HEARTBEAT] and clock.waiting == [], "the heartbeat's sleep was cancelled"


@pytest.mark.anyio
async def test_a_repeated_message_id_is_not_run_twice_and_gets_the_first_reply(spec):
    """Contract, Replies: a repeated messageId is the same turn. A first turn is resent with no contextId, so the
    repeat is a new context; it must still hold the conversation, for the turn after it.
    """
    seen = []
    async with serve(spec, scripted(seen)) as client:
        _, first = await say(client, "hello", message_id="m-1")
        _, again = await say(client, "hello", message_id="m-1")
        _, same_context = await say(client, "hello", context_id=first.context_id, message_id="m-1")
        _, next_turn = await say(client, "next", context_id=again.context_id)
    assert len(seen) == 2, "the model ran for the first send and for the next turn, not for the repeats"
    assert [t.status.state for t in (again, same_context)] == [DONE, DONE]
    assert artifact_text(again) == artifact_text(same_context) == artifact_text(first) == "Turn 1. You said: hello"
    assert artifact_text(next_turn) == "Turn 2. You said: next"


@pytest.mark.anyio
async def test_a_repeat_that_arrives_while_the_first_still_runs_waits_for_it(spec):
    seen, release = [], asyncio.Event()
    async with serve(spec, scripted(seen, release)) as client:
        first = asyncio.create_task(say(client, "hold", message_id="m-1"))
        await until(lambda: len(seen) == 1, "the model to start")
        again = asyncio.create_task(say(client, "hold", message_id="m-1"))
        for _ in range(500):  # the repeat is a task of its own once it arrives
            if len((await client.list_tasks(ListTasksRequest())).tasks) == 2:
                break
            await asyncio.sleep(0.01)
        assert not again.done(), "the repeat waits for the first run"
        release.set()
        await first
        _, repeated = await again
    assert len(seen) == 1 and artifact_text(repeated) == "held" and repeated.status.state == DONE


async def task_ids(client, count):
    """The ids of the tasks the agent holds, once there are `count` of them."""
    for _ in range(500):
        ids = [t.id for t in (await client.list_tasks(ListTasksRequest())).tasks]
        if len(ids) == count:
            return ids
        await asyncio.sleep(0.01)
    raise AssertionError(f"never {count} tasks")


@pytest.mark.anyio
async def test_a_repeat_that_waits_for_the_first_run_sends_a_heartbeat_too(spec, clock):
    """Contract, Replies: an event at least every 5 minutes, on the repeat's stream as well as the first's."""
    seen, release = [], asyncio.Event()
    async with serve(spec, scripted(seen, release)) as client:
        first = asyncio.create_task(say(client, "hold", message_id="m-1"))
        await until(lambda: len(seen) == 1 and len(clock.requested) == 1, "the first run and its heartbeat")
        again = asyncio.create_task(say(client, "hold", message_id="m-1"))
        await until(lambda: len(clock.requested) == 2, "the repeat's heartbeat")
        clock.tick()  # the first run's beat is the older sleeper, so it gets the first tick
        clock.tick()
        await until(lambda: len(clock.requested) == 4, "both heartbeats")
        release.set()
        await first
        events, _ = await again
    assert len(working(events)) == 2, "one `working` when the repeat began, and one heartbeat"


@pytest.mark.anyio
async def test_a_repeat_runs_again_when_the_run_it_waited_for_is_cancelled(spec):
    seen, release = [], asyncio.Event()
    async with serve(spec, scripted(seen, release)) as client:
        first = asyncio.create_task(say(client, "hold", message_id="m-1"))
        await until(lambda: len(seen) == 1, "the model to start")
        [first_id] = await task_ids(client, 1)
        again = asyncio.create_task(say(client, "hold", message_id="m-1"))
        await task_ids(client, 2)
        await client.cancel_task(CancelTaskRequest(id=first_id))
        _, canceled = await first
        release.set()
        _, repeated = await again
    assert canceled.status.state == CANCELED
    assert len(seen) == 2 and repeated.status.state == DONE and artifact_text(repeated) == "held"


@pytest.mark.anyio
async def test_cancelling_a_waiting_repeat_leaves_the_first_run_and_later_repeats_intact(spec):
    """The repeat waits on a future the first run owns: cancelling the repeat must not cancel that future."""
    seen, release = [], asyncio.Event()
    async with serve(spec, scripted(seen, release)) as client:
        first = asyncio.create_task(say(client, "hold", message_id="m-1"))
        await until(lambda: len(seen) == 1, "the model to start")
        [first_id] = await task_ids(client, 1)
        again = asyncio.create_task(say(client, "hold", message_id="m-1"))
        repeat_id = next(i for i in await task_ids(client, 2) if i != first_id)
        await client.cancel_task(CancelTaskRequest(id=repeat_id))
        _, canceled = await again
        release.set()
        _, done = await first
        _, third = await say(client, "hold", message_id="m-1")
    assert canceled.status.state == CANCELED
    assert done.status.state == DONE and artifact_text(done) == "held"
    assert third.status.state == DONE and artifact_text(third) == "held" and len(seen) == 1


@pytest.mark.anyio
async def test_a_repeated_message_id_runs_again_when_the_first_run_failed(spec):
    seen = []
    async with serve(spec, scripted(seen)) as client:
        _, first = await say(client, "boom", message_id="m-1")
        _, again = await say(client, "boom", message_id="m-1")
    assert len(seen) == 2 and first.status.state == again.status.state == FAILED


@pytest.mark.anyio
async def test_only_the_most_recent_contexts_are_kept(spec, monkeypatch):
    """State is in memory and bounded: past MAX_KEPT contexts the oldest is forgotten and its thread starts over."""
    monkeypatch.setattr(agent, "MAX_KEPT", 2)
    async with serve(spec, scripted()) as client:
        _, a = await say(client, "a")
        _, b = await say(client, "b")
        _, c = await say(client, "c")
        _, a2 = await say(client, "a again", context_id=a.context_id)
        _, c2 = await say(client, "c again", context_id=c.context_id)
    assert artifact_text(a2) == "Turn 1. You said: a again", "context a was forgotten"
    assert artifact_text(c2) == "Turn 2. You said: c again"
    assert b.context_id != c.context_id


@pytest.mark.anyio
async def test_list_tasks_finds_a_turn_by_its_message_id(spec):
    """Contract, Methods: ListTasks by contextId; a turn is found if a task's history holds its messageId."""
    async with serve(spec, scripted()) as client:
        _, task = await say(client, "hello", message_id="m-7")
        listed = (await client.list_tasks(ListTasksRequest(context_id=task.context_id))).tasks
    assert [m.message_id for t in listed for m in t.history] == ["m-7"]


TOKEN_HEADER = {"Authorization": "Bearer " + TOKEN}


@pytest.mark.anyio
@pytest.mark.parametrize("path", ["/.well-known/agent-card.json", "/"])
@pytest.mark.parametrize("header", [None, "Bearer wrong-token", TOKEN, "Basic " + TOKEN, "Bearer "])
async def test_with_a_token_file_a_missing_or_wrong_bearer_gets_401(spec, tmp_path, path, header):
    """Contract, Errors: HTTP 401 is a rejected credential. The card is fetched with the same identity as the calls."""
    token_file = tmp_path / "token"
    token_file.write_text(TOKEN + "\n")  # a trailing newline in the file is not part of the token
    async with asgi(spec, scripted(), str(token_file), {"Authorization": header} if header else None) as http:
        response = await http.get(path) if path != "/" else await http.post("/", json={"jsonrpc": "2.0", "id": 1})
    assert response.status_code == 401


@pytest.mark.anyio
async def test_with_a_token_file_the_right_bearer_is_served_and_a_rotated_token_needs_no_restart(spec, tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text(TOKEN)
    card = "/.well-known/agent-card.json"
    async with asgi(spec, scripted(), str(token_file), TOKEN_HEADER) as http:
        assert (await http.get(card)).status_code == 200
        token_file.write_text("rotated-fake-token\n")
        assert (await http.get(card)).status_code == 401
        assert (await http.get(card, headers={"Authorization": "Bearer rotated-fake-token"})).status_code == 200
        token_file.write_text("")  # an empty file at request time admits nobody, not even a bare "Bearer "
        assert (await http.get(card, headers={"Authorization": "Bearer "})).status_code == 401
        token_file.unlink()
        assert (await http.get(card)).status_code == 401


@pytest.mark.anyio
async def test_without_a_token_file_every_caller_is_accepted(spec):
    async with asgi(spec, scripted()) as http:
        assert (await http.get("/.well-known/agent-card.json")).status_code == 200


@pytest.mark.anyio
async def test_anything_but_http_and_lifespan_without_the_bearer_is_closed(spec, tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text(TOKEN)
    sent = []

    async def send(message):
        sent.append(message)

    await agent.build_app(URL, spec, str(token_file), scripted())({"type": "websocket", "headers": []}, None, send)
    assert sent == [{"type": "websocket.close"}]


def test_an_empty_token_file_stops_the_agent_instead_of_accepting_a_bare_bearer(spec, tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text("\n")
    with pytest.raises(SystemExit):
        agent.build_app(URL, spec, str(token_file), scripted())


MANIFESTS = HERE / "manifests"


def test_the_configmap_holds_the_example_agent_yaml():
    held = yaml.safe_load((MANIFESTS / "configmap.yaml").read_text())["data"]["agent.yaml"]
    assert held == (HERE / "agent.yaml").read_text()


def test_the_deployment_takes_every_variable_the_example_expands_from_a_secret_and_no_manifest_is_a_secret():
    deployment = yaml.safe_load((MANIFESTS / "deployment.yaml").read_text())
    env = {e["name"]: e for e in deployment["spec"]["template"]["spec"]["containers"][0]["env"]}
    mcp = yaml.safe_load((HERE / "agent.yaml").read_text())["capabilities"][0]["MCP"]
    assert (names := agent.ENV.findall(mcp["authorization_token"])), "the example expands a variable"
    for name in names:
        assert "secretKeyRef" in env[name]["valueFrom"], f"{name} is not read from a Secret"
    assert env["PROMPT_AGENT_SPEC"]["value"] == "/etc/prompt-agent/agent.yaml"
    kinds = {doc["kind"] for f in MANIFESTS.glob("*.yaml") for doc in yaml.safe_load_all(f.read_text())}
    assert kinds == {"ConfigMap", "Deployment", "NetworkPolicy", "Service"}
