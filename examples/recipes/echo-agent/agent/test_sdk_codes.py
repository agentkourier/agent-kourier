"""What a2a-sdk answers when a call names a finished task. These pin the SDK, not agent.py: read before upgrading.

Agent Kourier reads JSON-RPC -32004 on a send as "busy" and sends the message again (agent-contract.md, Errors), and
the SDK's UnsupportedOperationError is -32004 too. Agent Kourier names a task in a send only when it answers a
question, and on -32004 there it reads the task with GetTask and drops the answer if the task has ended. These tests
record the codes a2a-sdk 1.2.2 answers, so that an upgrade that changes one fails here and not in a thread:

  send naming a completed task   -32004
  send naming a canceled task    -32004
  CancelTask on a terminal task  -32002   (task not cancelable)
  SubscribeToTask on a terminal  -32004   (Agent Kourier subscribes only after GetTask shows the task running)
  any of the above, unknown task -32001   (task not found)

A send that names a context but no task starts a new task and succeeds, so a normal next turn is unaffected.
"""

import asyncio
import uuid

import pytest
from a2a.types.a2a_pb2 import CancelTaskRequest

from harness import CANCELED, DONE, error_code, final, rpc, running_task, say, serve


async def finished(client, gate, how):
    """A completed or canceled task, to aim calls at."""
    if how == "completed":
        gate.set()
        return final(await say(client, "hello"))
    stream = asyncio.create_task(say(client, "a reply that takes a little while"))
    task = await running_task(client)
    await client.cancel_task(CancelTaskRequest(id=task.id))
    canceled = final(await stream)
    gate.set()  # later turns in the test are not held
    return canceled


def send_params(task_id, **ids):
    """A SendStreamingMessage naming a task, as JSON-RPC params."""
    message = {"messageId": str(uuid.uuid4()), "role": "ROLE_USER", "parts": [{"text": "again"}], "taskId": task_id}
    return {"message": message | ids}


@pytest.mark.anyio
@pytest.mark.parametrize("how", ["completed", "canceled"])
async def test_finished_task_a_send_naming_a_finished_task_answers_32004(gate, how):
    async with serve() as (http, client):
        task = await finished(client, gate, how)
        assert task.status.state == (DONE if how == "completed" else CANCELED)
        by_task = await rpc(http, "SendStreamingMessage", send_params(task.id))
        by_both = await rpc(http, "SendStreamingMessage", send_params(task.id, contextId=task.context_id))
        new_turn = await say(client, "next turn", context_id=task.context_id)
    assert (error_code(by_task), error_code(by_both)) == (-32004, -32004)
    assert final(new_turn).status.state == DONE


@pytest.mark.anyio
@pytest.mark.parametrize("how", ["completed", "canceled"])
async def test_finished_task_cancel_task_on_a_finished_task_answers_32002_not_32004(gate, how):
    async with serve() as (http, client):
        task = await finished(client, gate, how)
        response = await rpc(http, "CancelTask", {"id": task.id})
    assert error_code(response) == -32002


@pytest.mark.anyio
async def test_finished_task_subscribe_to_a_finished_task_answers_32004_and_unknown_tasks_32001(gate):
    async with serve() as (http, client):
        task = await finished(client, gate, "completed")
        subscribe = await rpc(http, "SubscribeToTask", {"id": task.id})
        unknown = [await rpc(http, m, {"id": "no-such-task"}) for m in ("GetTask", "CancelTask", "SubscribeToTask")]
        unknown.append(await rpc(http, "SendStreamingMessage", send_params("no-such-task")))
    assert error_code(subscribe) == -32004
    assert [error_code(r) for r in unknown] == [-32001] * 4
