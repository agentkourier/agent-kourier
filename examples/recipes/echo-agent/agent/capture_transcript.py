"""Print one SendStreamingMessage exchange with a running echo agent, the way the recipe's README shows it.

    python agent.py &                                  # or the container, or any URL the agent answers on
    python capture_transcript.py [URL] [TEXT]          # default: http://localhost:8080 "hello"

The request is what Agent Kourier sends: one user message with one text part. The events are the agent's own, one per
line. Generated ids and times are replaced with fixed placeholders, so the output is the same on every run and the
README can be regenerated from the running agent instead of edited by hand.
"""

import json
import re
import sys

import httpx

url = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080"
text = sys.argv[2] if len(sys.argv) > 2 else "hello"
request = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "SendStreamingMessage",
    "params": {"message": {"messageId": "msg-1", "role": "ROLE_USER", "parts": [{"text": text}]}},
}

with httpx.stream("POST", url, json=request, headers={"A2A-Version": "1.0"}, timeout=30) as response:
    response.raise_for_status()
    events = [line.removeprefix("data: ") for line in response.iter_lines() if line.startswith("data: ")]

first = json.loads(events[0])["result"]["task"]  # the first event is the Task, which names the task and its context
placeholders = {first["id"]: "<task-id>", first["contextId"]: "<context-id>"}
print(f"> {json.dumps(request)}")
for event in events:
    for generated, fixed in placeholders.items():
        event = event.replace(generated, fixed)
    print("< " + re.sub(r"\d{4}-\d\d-\d\dT[\d:.]+Z", "2026-01-01T00:00:00Z", json.dumps(json.loads(event))))
