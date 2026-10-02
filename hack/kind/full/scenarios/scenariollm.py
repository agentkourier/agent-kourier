"""The scenario model of the full tier: an OpenAI-compatible chat model that plays the part of each Phase 1
scenario's agent, so the scenarios run against kagent as they run against the stub agent in the fast tier.

Which agent it plays is named by the X-Scenario header of the agent's ModelConfig (defaultHeaders), one per
scenario agent in scenarios.yaml; everything else comes from the conversation kagent sends it:

  gated   "Holding on gate G.", holds until POST /_control/release/G, then " Released.". G is the word after
          "gate:" in the last user message; with none the reply is "No gate named, not holding.". This is the
          stub agent's scenario-gated (S1, S4): the held task keeps the kagent task WORKING and the reply
          streaming, so what a thread shows while it is held does not depend on timing.
  memory  "Message N. Earlier: "a", "b"." where N counts the user messages of the conversation and the
          earlier ones are quoted (S2): the context kagent keeps across courier's restart is what it shows.
  ask     asks two questions with ask_user, a multiple choice and a free-text one, and after the answer says
          "Answers: ..." with what it was given, an empty one as (none) (S3).
  approve calls the MCP tool add_numbers(3, 5), which needs approval; "I have added 3 and 5. The result is 8."
          if it ran, and "Resumed after the tool result." if the call was rejected (S11).

It logs the shape of each request, the roles and the first words of each message and never a header, so what
kagent sends a model can be read from its log (the full tier's findings come from there). Test only.
"""
import json
import re
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

GATE = re.compile(r"gate:([\w-]+)")
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


PERSON = "[Slack: "  # courier's attribution, which kagent keeps at the start of what a person wrote


def users(messages):
    """What people wrote, oldest first. kagent replays the agent's own earlier replies to its model as user
    messages too (measured), and the only way to tell them from a person's is courier's attribution prefix."""
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
    """What the model does with one request: some text, held on a gate after its first part, or one tool call."""

    def __init__(self, parts=(), hold=None, tool=None):
        self.parts, self.hold, self.tool = list(parts), hold, tool


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
            gate(script.hold).wait()
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

        if script.tool:
            name, args = script.tool
            send({"role": "assistant", "content": None, "tool_calls": [{"index": 0, "id": "call_scenario", "type": "function",
                                                                          "function": {"name": name, "arguments": json.dumps(args)}}]})
            send({}, "tool_calls")
        else:
            for i, part in enumerate(script.parts):
                send({"role": "assistant", "content": part})
                if script.hold and i == 0:
                    gate(script.hold).wait()
            send({}, "stop")
        end = b"data: [DONE]\n\n"
        self.wfile.write(b"%x\r\n%s\r\n0\r\n\r\n" % (len(end), end))
        self.wfile.flush()


def completion(script):
    if script.tool:
        name, args = script.tool
        message = {"role": "assistant", "content": None, "tool_calls": [{"id": "call_scenario", "type": "function",
                                                                          "function": {"name": name, "arguments": json.dumps(args)}}]}
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
