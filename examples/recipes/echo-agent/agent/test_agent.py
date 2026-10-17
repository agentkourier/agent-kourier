"""Tests for agent.py, driven through the a2a-sdk's own client over an in-process ASGI transport: no port, no network.

Each test names the row of docs/reference/agent-contract.md it proves. test_sdk_codes.py pins what the SDK itself
answers to calls that name a finished task.
"""

import asyncio

import pytest
from a2a.types.a2a_pb2 import CancelTaskRequest, GetTaskRequest, ListTasksRequest, SubscribeToTaskRequest

import agent
from harness import (
    CANCELED,
    DONE,
    FAILED,
    INPUT,
    URL,
    artifact_text,
    asgi,
    collect,
    error_code,
    final,
    rpc,
    running_task,
    say,
    serve,
)

TOKEN = "fake-test-token"  # not a secret: a constant for the bearer tests
ACTOR = "agentkourier.dev/actor"


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
async def test_cancel_in_the_middle_of_a_stream_ends_the_task_canceled_and_stops_the_reply(gate):
    """Contract, Methods: CancelTask. The task ends canceled and the rest of the reply is never sent."""
    async with serve() as (_, client):
        stream = asyncio.create_task(say(client, "a reply that takes a little while"))
        task = await running_task(client)
        canceled = await client.cancel_task(CancelTaskRequest(id=task.id))
        events = await stream
    assert canceled.status.state == CANCELED
    assert final(events).status.state == CANCELED
    assert len([e for e in events if e.HasField("artifact_update")]) < agent.CHUNKS


@pytest.mark.anyio
async def test_get_task_list_tasks_and_subscribe_answer_for_a_known_task(gate):
    """Contract, Methods: GetTask, ListTasks (with the turn's messageId in history) and SubscribeToTask."""
    async with serve() as (_, client):
        stream = asyncio.create_task(say(client, "keep going"))
        running = await running_task(client)
        subscription = asyncio.create_task(collect(client.subscribe(SubscribeToTaskRequest(id=running.id))))
        for _ in range(50):  # in-process there is no I/O: a few turns of the loop let the subscription attach
            await asyncio.sleep(0)
        gate.set()
        resumed = await subscription
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


@pytest.mark.anyio
async def test_a_rotated_token_works_without_a_restart_and_the_old_one_stops_working(tmp_path):
    """The token file is read on every request, so rotating the Binding's token needs no restart."""
    token_file = tmp_path / "token"
    token_file.write_text(TOKEN)
    async with asgi(str(token_file)) as http:
        card = "/.well-known/agent-card.json"
        old, new = {"Authorization": "Bearer " + TOKEN}, {"Authorization": "Bearer rotated-fake-token"}
        assert (await http.get(card, headers=old)).status_code == 200
        token_file.write_text("rotated-fake-token\n")
        assert (await http.get(card, headers=new)).status_code == 200
        assert (await http.get(card, headers=old)).status_code == 401
        token_file.write_text("")  # an empty file at request time admits nobody, not even a bare "Bearer "
        assert (await http.get(card, headers=new)).status_code == 401
        assert (await http.get(card, headers={"Authorization": "Bearer "})).status_code == 401
        token_file.unlink()
        assert (await http.get(card, headers=new)).status_code == 401


@pytest.mark.anyio
async def test_anything_but_http_and_lifespan_without_the_bearer_is_closed(tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text(TOKEN)
    sent = []

    async def send(message):
        sent.append(message)

    app = agent.build_app(URL, str(token_file))
    await app({"type": "websocket", "headers": []}, None, send)
    assert sent == [{"type": "websocket.close"}]


def test_an_empty_token_file_stops_the_agent_instead_of_accepting_a_bare_bearer(tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text("\n")
    with pytest.raises(SystemExit):
        agent.build_app(URL, str(token_file))
