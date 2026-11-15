"""Tests for fake_model.py, the model of the fast kind tier's e2e, behind the real agent: the agent builds the spec's
model, openai-chat:fake, and Pydantic AI's OpenAI client streams from the fake over HTTP with OPENAI_BASE_URL, as the
pod in the tier does. The MCP server is the real test one (mcp_server.py). Each test is one case the e2e relies on.
"""

import pytest
from a2a.types.a2a_pb2 import CancelTaskRequest

from harness import CANCELED, DONE, INPUT, artifact_text, fake_model, say, serve, spec_file

RUNBOOK = "Runbook for DiskFull: check the pod's recent restarts, then its memory limit."


@pytest.fixture(scope="session")
def fake_url():
    with fake_model() as url:
        yield url


@pytest.fixture
def spec(tmp_path, server, fake_url, monkeypatch):
    """agent.yaml with the fake as its model, and the variables the pod sets for it."""
    monkeypatch.setenv("OPENAI_BASE_URL", fake_url)
    monkeypatch.setenv("OPENAI_API_KEY", "fake-model-key")  # the OpenAI client needs one; the fake reads none
    return spec_file(tmp_path, server.url, model="openai-chat:fake", allowed_tools=["runbook"])


@pytest.mark.anyio
async def test_a_runbook_prompt_calls_the_mcp_tool_and_streams_an_answer_that_quotes_it(spec, server):
    async with serve(spec, None) as client:
        events, task = await say(client, "runbook for DiskFull")
    assert task.status.state == DONE
    assert artifact_text(task) == f"The runbook says: {RUNBOOK}"
    assert len([e for e in events if e.HasField("artifact_update")]) > 3, "the answer streams in pieces"
    assert [(c["tool"], c.get("alert")) for c in server.calls()] == [("runbook", "DiskFull")]


@pytest.mark.anyio
async def test_a_later_turn_quotes_the_first_one_of_its_context_only(spec):
    async with serve(spec, None) as client:
        _, first = await say(client, "runbook for DiskFull")
        _, second = await say(client, "what did I ask first?", context_id=first.context_id)
        _, other = await say(client, "hello")
    assert artifact_text(second) == "Messages from you in this conversation: 2. The first: runbook for DiskFull"
    assert artifact_text(other) == "Messages from you in this conversation: 1. The first: hello"


@pytest.mark.anyio
async def test_ask_me_pauses_with_a_question_and_the_answer_completes_the_task(spec):
    async with serve(spec, None) as client:
        _, question = await say(client, "ask me which environment")
        assert question.status.state == INPUT
        assert [p.text for p in question.status.message.parts] == ["Which environment should I check?"]
        _, answer = await say(client, "staging", context_id=question.context_id, task_id=question.id)
    assert (answer.id, answer.status.state) == (question.id, DONE)
    assert artifact_text(answer) == "You answered: staging"


@pytest.mark.anyio
async def test_after_a_cancelled_question_the_next_turn_is_answered_with_the_context_kept(spec):
    async with serve(spec, None) as client:
        _, question = await say(client, "ask me which environment")
        canceled = await client.cancel_task(CancelTaskRequest(id=question.id))
        _, after = await say(client, "are you there?", context_id=question.context_id)
    assert canceled.status.state == CANCELED
    assert after.status.state == DONE
    assert artifact_text(after) == "Messages from you in this conversation: 2. The first: ask me which environment"
