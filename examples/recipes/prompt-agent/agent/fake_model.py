"""A deterministic OpenAI-compatible model for the fast kind tier's e2e: streamed chat completions, with tool calls.

Not part of the recipe's image. The tier runs it as a pod, and the prompt agent reaches it with model: openai-chat:fake
and OPENAI_BASE_URL=http://<host>/v1; test_fake_model.py runs it as a subprocess behind the real agent. Standard library
only. It answers POST /v1/chat/completions with stream: true, as Pydantic AI sends it, by these rules, read from the
request alone (no state of its own):

- the last message is a tool result: "The runbook says: <result>" for runbook, "You answered: <answer>" for ask_user;
- the person's last message holds "runbook for NAME": a call of runbook with alert NAME;
- it holds "ask me": a call of ask_user with the question "Which environment should I check?";
- anything else: "Messages from you in this conversation: N. The first: <first>", which only the history can tell.

Text streams a word at a time, FAKE_MODEL_DELAY seconds apart (0.02 unless set), so a reply arrives as many appends.
"""

import json
import os
import re
import socketserver
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

QUESTION = "Which environment should I check?"
DELAY = float(os.environ.get("FAKE_MODEL_DELAY", "0.02"))


def words(content) -> str:
    """A message's text: a string, or the text parts of a list."""
    if isinstance(content, list):
        return "".join(part.get("text", "") for part in content if isinstance(part, dict))
    return content or ""


def answer(messages: list[dict]) -> tuple[str, dict | None]:
    """The reply to a request: (text, None), or ("", a tool call)."""
    last = messages[-1]
    if last.get("role") == "tool":
        calls = {c["id"]: c["function"]["name"] for m in messages for c in m.get("tool_calls") or []}
        result = words(last.get("content"))
        if calls.get(last.get("tool_call_id")) == "ask_user":
            return f"You answered: {result}", None
        return f"The runbook says: {result}", None
    said = [words(m.get("content")) for m in messages if m.get("role") == "user"]
    if found := re.search(r"runbook for (\S+)", said[-1]):
        return "", {"name": "runbook", "arguments": {"alert": found.group(1)}}
    if "ask me" in said[-1]:
        return "", {"name": "ask_user", "arguments": {"question": QUESTION}}
    return f"Messages from you in this conversation: {len(said)}. The first: {said[0]}", None


def chunks(text: str, call: dict | None, usage: bool):
    """The stream's chunks: the role, then text a word at a time or one tool call, then the finish (and usage)."""
    base = {"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": int(time.time()), "model": "fake"}

    def chunk(delta, finish=None):
        return {**base, "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}

    yield chunk({"role": "assistant", "content": ""})
    if call:
        function = {"name": call["name"], "arguments": json.dumps(call["arguments"])}
        tool_call = {"index": 0, "id": f"call_{call['name']}", "type": "function", "function": function}
        yield chunk({"tool_calls": [tool_call]})
        yield chunk({}, "tool_calls")
    else:
        for piece in re.findall(r"\S+\s*", text):
            yield chunk({"content": piece})
        yield chunk({}, "stop")
    if usage:
        yield {**base, "choices": [], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self) -> None:
        if self.path.rstrip("/") not in ("/v1/chat/completions", "/chat/completions"):
            return self.send_error(404)
        request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        if not request.get("stream"):
            return self.send_error(400, "only stream: true is served")
        text, call = answer(request["messages"])
        print(f"fake model: {'call ' + call['name'] if call else 'text'}", flush=True)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")  # the body ends when the connection does: no length, no chunking
        self.end_headers()
        for c in chunks(text, call, bool((request.get("stream_options") or {}).get("include_usage"))):
            self.wfile.write(f"data: {json.dumps(c)}\n\n".encode())
            self.wfile.flush()
            time.sleep(DELAY)  # a stream, not one write: the agent sees the pieces arrive apart
        self.wfile.write(b"data: [DONE]\n\n")
        self.close_connection = True

    def log_message(self, *args) -> None:  # one line per request, from do_POST, and no access log
        pass


class Server(ThreadingHTTPServer):
    def server_bind(self) -> None:
        # HTTPServer's own also looks up the host's name (socket.getfqdn), which can wait 30 seconds on a reverse DNS
        # lookup; the name is never used here.
        socketserver.TCPServer.server_bind(self)


if __name__ == "__main__":
    # Loopback unless FAKE_MODEL_HOST says otherwise: the tier's pod sets 0.0.0.0, so that its Service reaches it.
    host = os.environ.get("FAKE_MODEL_HOST", "127.0.0.1")
    Server((host, int(sys.argv[1])), Handler).serve_forever()
