"""Tests for agent.py, driven through the a2a-sdk's own client over an in-process ASGI transport: no port, no network.

Each test names the row of docs/reference/agent-contract.md it proves. The finished-task tests pin what the SDK
answers to calls that name a finished task; read the comment above them before upgrading a2a-sdk.
"""

import asyncio
import contextlib
import uuid

import httpx
import pytest
from a2a.client import ClientConfig, ClientFactory
from a2a.types.a2a_pb2 import (
    CancelTaskRequest,
    GetTaskRequest,
    ListTasksRequest,
    Message,
    Part,
    Role,
    SendMessageRequest,
    SubscribeToTaskRequest,
    TaskState,
)

import agent

URL = "http://echo-agent.test"
TOKEN = "fake-test-token"  # not a secret: a constant for the bearer tests
DONE, WORKING = TaskState.TASK_STATE_COMPLETED, TaskState.TASK_STATE_WORKING
FAILED, CANCELED = TaskState.TASK_STATE_FAILED, TaskState.TASK_STATE_CANCELED
INPUT = TaskState.TASK_STATE_INPUT_REQUIRED
ACTOR = "agentkourier.dev/actor"


@pytest.fixture
def anyio_backend():
    return "asyncio"


@pytest.fixture(autouse=True)
def no_delay(monkeypatch):
    monkeypatch.setattr(agent, "CHUNK_DELAY", 0)


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


@pytest.mark.anyio
async def test_card_is_served_with_streaming_and_a_jsonrpc_interface():
    """Contract, Discovery and transport: card at the well-known path, streaming on, JSON-RPC, no required extension."""
    async with serve() as (http, _):
        card = (await http.get("/.well-known/agent-card.json")).json()
    assert card["capabilities"]["streaming"] is True
    assert [(i["protocolBinding"], i["protocolVersion"]) for i in card["supportedInterfaces"]] == [("JSONRPC", "1.0")]
    assert not [e for e in card["capabilities"].get("extensions", []) if e.get("required")]


@pytest.mark.anyio
async def test_a_reply_streams_in_pieces_and_ends_with_a_full_completed_task():
    """Contract, Replies: task events, a Task first and a full Task last, and the answer in artifact text parts."""
    async with serve() as (_, client):
        events = await say(client, "hello")
    assert events[0].HasField("task"), "the first event names the task"
    pieces = [e.artifact_update for e in events if e.HasField("artifact_update")]
    assert len(pieces) > 1, "the reply streams in several pieces"
    assert [p.append for p in pieces] == [False] + [True] * (len(pieces) - 1)
    task = final(events)
    assert task.status.state == DONE
    assert task.context_id and task.id
    assert all(p.task_id == task.id and p.context_id == task.context_id for p in pieces)
    assert artifact_text(task).endswith("You said: hello")
    assert "".join(part.text for p in pieces for part in p.artifact.parts) == artifact_text(task)


@pytest.mark.anyio
async def test_a_second_turn_keeps_the_context_names_turn_2_and_quotes_turn_1():
    """Contract, Replies: create a contextId on the first turn and keep it for every later turn."""
    async with serve() as (_, client):
        first = final(await say(client, "one"))
        second = final(await say(client, "two", context_id=first.context_id))
        other = final(await say(client, "elsewhere"))
    assert second.context_id == first.context_id
    assert second.id != first.id, "each turn is a task of its own"
    assert "Turn 1. You" in artifact_text(first)
    assert "Turn 2. You" in artifact_text(second)
    assert artifact_text(second).endswith("Before that you said: one")
    assert "Turn 1. You" in artifact_text(other)


@pytest.mark.anyio
async def test_only_the_newest_contexts_are_remembered(monkeypatch):
    """The agent keeps its state in memory, bounded."""
    monkeypatch.setattr(agent, "MAX_CONTEXTS", 2)
    async with serve() as (_, client):
        oldest = final(await say(client, "a"))
        await say(client, "b")
        await say(client, "c")
        forgotten = final(await say(client, "again", context_id=oldest.context_id))
    assert "Turn 1. You" in artifact_text(forgotten)


@pytest.mark.anyio
async def test_a_question_pauses_in_input_required_with_text_only_and_the_answer_completes_the_same_task():
    """Contract, Task states: a question is input-required with text parts only; the answer continues the task."""
    async with serve() as (_, client):
        question = final(await say(client, "ask"))
        assert question.status.state == INPUT
        parts = question.status.message.parts
        assert parts and all(p.WhichOneof("content") == "text" and p.text for p in parts)
        assert question.status.message.message_id
        answer = final(await say(client, "42", context_id=question.context_id, task_id=question.id))
    assert (answer.id, answer.context_id) == (question.id, question.context_id)
    assert answer.status.state == DONE
    assert artifact_text(answer) == "You answered: 42"


@pytest.mark.anyio
async def test_fail_ends_the_task_failed_with_a_status_message():
    """Contract, Task states: failed ends the turn; Agent Kourier reads the status message's text."""
    async with serve() as (_, client):
        task = final(await say(client, "fail"))
    assert task.status.state == FAILED
    assert task.status.message.parts[0].text


@pytest.mark.anyio
async def test_help_lists_the_commands_and_the_actor_prefix_and_bot_mention_are_not_part_of_the_command():
    """Contract, What Agent Kourier sends: a person's turn has a prefix line and the actor in its metadata."""
    actor = {ACTOR: {"id": "U123", "platform": "slack", "name": "Ada"}}
    async with serve() as (_, client):
        task = final(await say(client, "[Slack: Ada (U123)]\n<@UBOT> help", metadata=actor))
        echoed = final(await say(client, "no actor here\nask"))  # no metadata: nothing is stripped
    assert artifact_text(task) == agent.HELP
    assert echoed.status.state == DONE and "no actor here" in artifact_text(echoed)


@pytest.mark.anyio
async def test_cancel_in_the_middle_of_a_stream_ends_the_task_canceled_and_stops_the_reply(monkeypatch):
    """Contract, Methods: CancelTask. The task ends canceled and the rest of the reply is never sent."""
    monkeypatch.setattr(agent, "CHUNK_DELAY", 0.05)
    async with serve() as (_, client):
        stream = asyncio.create_task(say(client, "a reply that takes a little while"))
        task = await running_task(client)
        canceled = await client.cancel_task(CancelTaskRequest(id=task.id))
        events = await stream
    assert canceled.status.state == CANCELED
    assert final(events).status.state == CANCELED
    assert len([e for e in events if e.HasField("artifact_update")]) < agent.CHUNKS


@pytest.mark.anyio
async def test_get_task_list_tasks_and_subscribe_answer_for_a_known_task(monkeypatch):
    """Contract, Methods: GetTask, ListTasks (with the turn's messageId in history) and SubscribeToTask."""
    monkeypatch.setattr(agent, "CHUNK_DELAY", 0.05)
    async with serve() as (_, client):
        stream = asyncio.create_task(say(client, "keep going"))
        running = await running_task(client)
        resumed = [r async for r in client.subscribe(SubscribeToTaskRequest(id=running.id))]
        done = final(await stream)
        got = await client.get_task(GetTaskRequest(id=done.id))
        # The broker lists with the context and no history length; the turn must still be found by its messageId.
        listed = (await client.list_tasks(ListTasksRequest(context_id=done.context_id))).tasks
    assert final(resumed).status.state == DONE
    assert got.status.state == DONE and artifact_text(got) == artifact_text(done)
    assert [t.id for t in listed] == [done.id]
    assert [m.message_id for m in listed[0].history] == [m.message_id for m in done.history] != []


@pytest.mark.anyio
async def test_without_a_token_file_every_caller_is_accepted():
    async with asgi() as http:
        assert (await http.get("/.well-known/agent-card.json")).status_code == 200


@pytest.mark.anyio
@pytest.mark.parametrize("path", ["/.well-known/agent-card.json", "/"])
@pytest.mark.parametrize("header", [None, "Bearer wrong-token", TOKEN, "Basic " + TOKEN, "Bearer "])
async def test_with_a_token_file_a_missing_or_wrong_bearer_gets_401(tmp_path, path, header):
    """Contract, Errors: HTTP 401 is a rejected credential. The card is fetched with the same identity as the calls."""
    token_file = tmp_path / "token"
    token_file.write_text(TOKEN + "\n")  # a trailing newline in the file is not part of the token
    async with asgi(str(token_file), {"Authorization": header} if header else None) as http:
        response = await http.get(path) if path != "/" else await rpc(http, "GetTask", {"id": "x"})
    assert response.status_code == 401


@pytest.mark.anyio
async def test_with_a_token_file_the_right_bearer_is_served(tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text(TOKEN + "\n")
    async with asgi(str(token_file), {"Authorization": "Bearer " + TOKEN}) as http:
        assert (await http.get("/.well-known/agent-card.json")).status_code == 200
        assert error_code(await rpc(http, "GetTask", {"id": "x"})) == -32001  # past the check, to the SDK


def test_an_empty_token_file_stops_the_agent_instead_of_accepting_a_bare_bearer(tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text("\n")
    with pytest.raises(SystemExit):
        agent.build_app(URL, str(token_file))


# Finished tasks: the broker reads JSON-RPC -32004 on a send as "busy" and retries it for ever (agent-contract.md,
# Errors). The SDK's UnsupportedOperationError is -32004. These tests record what the SDK (a2a-sdk 1.2.2) answers when
# a call names a finished task, so that a bump which changes a code fails here and not in a thread.
#
#   send naming a completed task   -32004   <- the broker would retry this for ever
#   send naming a canceled task    -32004   <- the same
#   CancelTask on a terminal task  -32002   (task not cancelable)
#   SubscribeToTask on a terminal  -32004   (the broker subscribes only after GetTask shows the task running)
#   any of the above, unknown task -32001   (task not found)
#
# A send that names a context but no task starts a new task and succeeds, so a normal next turn is unaffected.


async def finished(client, how):
    """A completed or canceled task, to aim calls at."""
    if how == "completed":
        return final(await say(client, "hello"))
    stream = asyncio.create_task(say(client, "a reply that takes a little while"))
    task = await running_task(client)
    await client.cancel_task(CancelTaskRequest(id=task.id))
    return final(await stream)


def send_params(task_id, **ids):
    """A SendStreamingMessage naming a task, as JSON-RPC params."""
    message = {"messageId": str(uuid.uuid4()), "role": "ROLE_USER", "parts": [{"text": "again"}], "taskId": task_id}
    return {"message": message | ids}


@pytest.mark.anyio
@pytest.mark.parametrize("how", ["completed", "canceled"])
async def test_finished_task_a_send_naming_a_finished_task_answers_32004(monkeypatch, how):
    monkeypatch.setattr(agent, "CHUNK_DELAY", 0.05 if how == "canceled" else 0)
    async with serve() as (http, client):
        task = await finished(client, how)
        assert task.status.state == (DONE if how == "completed" else CANCELED)
        by_task = await rpc(http, "SendStreamingMessage", send_params(task.id))
        by_both = await rpc(http, "SendStreamingMessage", send_params(task.id, contextId=task.context_id))
        new_turn = await say(client, "next turn", context_id=task.context_id)
    assert (error_code(by_task), error_code(by_both)) == (-32004, -32004)
    assert final(new_turn).status.state == DONE


@pytest.mark.anyio
@pytest.mark.parametrize("how", ["completed", "canceled"])
async def test_finished_task_cancel_task_on_a_finished_task_answers_32002_not_32004(monkeypatch, how):
    monkeypatch.setattr(agent, "CHUNK_DELAY", 0.05 if how == "canceled" else 0)
    async with serve() as (http, client):
        task = await finished(client, how)
        response = await rpc(http, "CancelTask", {"id": task.id})
    assert error_code(response) == -32002


@pytest.mark.anyio
async def test_finished_task_subscribe_to_a_finished_task_answers_32004_and_unknown_tasks_32001():
    async with serve() as (http, client):
        task = await finished(client, "completed")
        subscribe = await rpc(http, "SubscribeToTask", {"id": task.id})
        unknown = [await rpc(http, m, {"id": "no-such-task"}) for m in ("GetTask", "CancelTask", "SubscribeToTask")]
        unknown.append(await rpc(http, "SendStreamingMessage", send_params("no-such-task")))
    assert error_code(subscribe) == -32004
    assert [error_code(r) for r in unknown] == [-32001] * 4
