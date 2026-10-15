"""The scenario model of the full tier: an OpenAI-compatible chat model that plays the part of each Phase 1
scenario's agent, so the scenarios run against kagent as they run against the stub agent in the fast tier.

Which agent it plays is named by the X-Scenario header of the agent's ModelConfig (defaultHeaders), one per
scenario agent in scenarios.yaml; everything else comes from the conversation kagent sends it:

  gated   "Holding on gate G.", holds until POST /_control/release/G, then " Released.". G is the word after
          "gate:" in the last user message; with none the reply is "No gate named, not holding.". This is the
          stub agent's scenario-gated (S1, S4): the held task keeps the kagent task WORKING and the reply
          streaming, so what a thread shows while it is held does not depend on timing.
  memory  "Message N. Earlier: "a", "b"." where N counts the user messages of the conversation and the
          earlier ones are quoted (S2): the context kagent keeps across Agent Kourier's restart is what it shows.
  ask     asks two questions with ask_user, a multiple choice and a free-text one, and after the answer says
          "Answers: ..." with what it was given, an empty one as (none) (S3).
  approve calls the MCP tool add_numbers(3, 5), which needs approval; "I have added 3 and 5. The result is 8."
          if it ran, and "Resumed after the tool result." if the call was rejected (S11).
  approval calls the same tool for the approval-enabled full-tier scenarios (S19-S22 and S24), then says it
            received the result. approval-multi emits add_numbers and check_status in one response with distinct IDs (S23).
  tools   says "Checking the service.", calls the MCP tool check_status(service="api"), says "It is up, so I will
          restart it.", calls restart_service(service="api"), which fails, and says "The restart was refused." Which
          of the three model calls this is comes from the tool results since the last user message (S13). This is the
          stub agent's scenario-tools: the reply has a step card for each call, done and error.

It logs the shape of each request, the roles and the first words of each message and never a header, so what
kagent sends a model can be read from its log (the full tier's findings come from there). Test only.
"""
import json
import os
import re
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

GATE = re.compile(r"gate:([\w-]+)")
# How long a gate holds a reply that nobody releases: a run that fails before it releases its gate must not leave
# a kagent task WORKING for ever. After it the reply goes on as if released.
GATE_TIMEOUT = float(os.environ.get("GATE_TIMEOUT_SECONDS", "600"))
gates = {}
gates_lock = threading.Lock()


def gate(name):
    with gates_lock:
        return gates.setdefault(name, threading.Event())


def text_of(content):
    """A message's content as text: a string, or the text parts of a list of parts."""
    if isinstance(content, list):
        return " ".join(p.get("text", "") for p in content if isinstance(p, dict))
    return content or ""


PERSON = "[Slack: "  # Agent Kourier's attribution, which kagent keeps at the start of what a person wrote


def users(messages):
    """What people wrote, oldest first. kagent replays the agent's own earlier replies to its model as user
    messages too (measured), and the only way to tell them from a person's is Agent Kourier's attribution prefix."""
    return [t for t in (text_of(m.get("content")) for m in messages if m.get("role") == "user") if t.startswith(PERSON)]


def quote(s):
    return json.dumps(s, ensure_ascii=False)


def answers_of(tool_text):
    """What ask_user's result says the person answered, one entry per question, in kagent's own shape if it can
    be read and as the raw text if not (the shape is what the log shows)."""
    try:
        data = json.loads(tool_text)
    except ValueError:
        return [tool_text]
    found = data.get("answers") if isinstance(data, dict) else data
    if not isinstance(found, list):
        return [tool_text]
    out = []
    for a in found:
        if isinstance(a, dict):
            a = a.get("answer", a.get("answers", []))
        out.append(", ".join(a) if isinstance(a, list) else str(a))
    return [e if e else "(none)" for e in out]


class Script:
    """What the model does with one request: text, an optional gate, and zero or more tool calls."""

    def __init__(self, parts=(), hold=None, tool=None, tools=None):
        if tool is not None and tools is not None:
            raise ValueError("Script accepts tool or tools, not both")
        self.parts, self.hold = list(parts), hold
        self.tools = list(tools if tools is not None else (() if tool is None else (tool,)))
        # Keep the old attribute for callers that inspect a one-tool Script.
        self.tool = self.tools[0] if len(self.tools) == 1 else None


def tool_results_since_user(messages):
    """How many tool results the conversation has after its last user message: how far the model is in its own turn."""
    n = 0
    for m in reversed(messages):
        if m.get("role") == "user":
            break
        if m.get("role") == "tool":
            n += 1
    return n


def play(scenario, messages):
    last = messages[-1] if messages else {}
    last_text = text_of(last.get("content"))
    if scenario == "gated":
        m = GATE.search(last_text)
        if not m:
            return Script(["No gate named, not holding."])
        return Script(["Holding on gate %s." % m.group(1), " Released."], hold=m.group(1))
    if scenario == "memory":
        prior = users(messages)[:-1]
        earlier = ", ".join(quote(p) for p in prior) if prior else "none"
        return Script(["Message %d. Earlier: %s." % (len(prior) + 1, earlier)])
    if scenario == "ask":
        if last.get("role") == "tool":
            return Script(["Answers: " + " | ".join(quote(a) if a != "(none)" else a for a in answers_of(last_text)) + "."])
        return Script(tool=("ask_user", {"questions": [
            {"question": "Which databases?", "choices": ["PostgreSQL", "MySQL"], "multiple": True},
            {"question": "Favorite color?"}]}))
    if scenario == "approve":
        if last.get("role") == "tool":
            return Script(["Resumed after the tool result." if "reject" in last_text.lower() else "I have added 3 and 5. The result is 8."])
        return Script(tool=("add_numbers", {"a": 3, "b": 5}))
    if scenario == "approval":
        if last.get("role") == "tool":
            return Script(["Approval result received."])
        return Script(tools=[("add_numbers", {"a": 3, "b": 5})])
    if scenario == "approval-multi":
        if last.get("role") == "tool":
            return Script(["Approval results received."])
        return Script(tools=[("add_numbers", {"a": 3, "b": 5}), ("check_status", {"service": "api"})])
    if scenario == "tools":
        done = tool_results_since_user(messages)
        if done == 0:
            return Script(["Checking the service."], tool=("check_status", {"service": "api"}))
        if done == 1:
            return Script(["It is up, so I will restart it."], tool=("restart_service", {"service": "api"}))
        return Script(["The restart was refused."])
    return Script(["The answer is 4."])


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def reply(self, code, body=b"", ctype="application/json"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)
        m = re.fullmatch(r"/_control/release/([\w-]+)", self.path)
        if m:
            gate(m.group(1)).set()
            return self.reply(204)
        try:
            req = json.loads(raw or b"{}")
        except ValueError:
            return self.reply(400, b'{"error":"not json"}')
        scenario = self.headers.get("X-Scenario", "")
        messages = req.get("messages", [])
        log_request(scenario, req, messages)
        script = play(scenario, messages)
        if req.get("stream"):
            return self.stream(script)
        if script.hold:
            gate(script.hold).wait(timeout=GATE_TIMEOUT)
        self.reply(200, json.dumps(completion(script)).encode())

    def stream(self, script):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        def send(delta, finish=None):
            payload = {"id": "chatcmpl-scenario", "object": "chat.completion.chunk", "created": 1, "model": "gpt-4.1-mini",
                       "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
            data = ("data: %s\n\n" % json.dumps(payload)).encode()
            self.wfile.write(b"%x\r\n%s\r\n" % (len(data), data))
            self.wfile.flush()

        for i, part in enumerate(script.parts):
            send({"role": "assistant", "content": part})
            if script.hold and i == 0:
                gate(script.hold).wait(timeout=GATE_TIMEOUT)
        if script.tools:
            calls = []
            for index, (name, args) in enumerate(script.tools):
                calls.append({"index": index, "id": call_id(name) if len(script.tools) == 1 else call_id(name, index),
                              "type": "function", "function": {"name": name, "arguments": json.dumps(args)}})
            send({"role": "assistant", "content": None, "tool_calls": calls})
            send({}, "tool_calls")
        else:
            send({}, "stop")
        end = b"data: [DONE]\n\n"
        self.wfile.write(b"%x\r\n%s\r\n0\r\n\r\n" % (len(end), end))
        self.wfile.flush()


def call_id(name, index=None):
    """A tool call's ID, its own for each tool: a turn that calls two tools must not give both one ID."""
    return "call_scenario_" + name if index is None else "call_scenario_%s_%d" % (name, index)


def completion(script):
    if script.tools:
        calls = []
        for index, (name, args) in enumerate(script.tools):
            calls.append({"id": call_id(name) if len(script.tools) == 1 else call_id(name, index), "type": "function",
                          "function": {"name": name, "arguments": json.dumps(args)}})
        message = {"role": "assistant", "content": "".join(script.parts) or None, "tool_calls": calls}
        finish = "tool_calls"
    else:
        message, finish = {"role": "assistant", "content": "".join(script.parts)}, "stop"
    return {"id": "chatcmpl-scenario", "object": "chat.completion", "created": 1, "model": "gpt-4.1-mini",
            "choices": [{"index": 0, "message": message, "finish_reason": finish}],
            "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}


def log_request(scenario, req, messages):
    shape = ["%s:%s" % (m.get("role"), text_of(m.get("content"))[:60].replace("\n", " ")) for m in messages]
    print(json.dumps({"scenario": scenario, "stream": bool(req.get("stream")), "tools": [t.get("function", {}).get("name") for t in req.get("tools", [])],
                      "messages": shape}), flush=True)


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
