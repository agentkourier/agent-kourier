"""A three-tool MCP server for the full tier's scenario agents. Streamable HTTP, JSON answers, and test counters.

  add_numbers(a, b)        S11's agent lists it with requireApproval, so kagent pauses the task for approval before
                           the tool runs, and Agent Kourier, on a Binding that does not allow approvals, denies it.
  check_status(service)    S13's agent calls it: it works ("<service> is up").
  restart_service(service) S13's agent calls it: it fails, as an MCP tool reports a failure, with isError true and the
                           reason as its text ("restart refused: <service> is locked").

The X-Scenario request header keys the in-memory execution counters. GET /_control/counters exposes a snapshot for
the full-tier tests, so a call from one approval scenario cannot be mistaken for a call from another one.
"""
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

counters = {}
counters_lock = threading.Lock()


def record_call(scenario, name):
    """Count an executed tools/call under its model scenario header."""
    with counters_lock:
        scenario_counts = counters.setdefault(scenario or "default", {})
        scenario_counts[name] = scenario_counts.get(name, 0) + 1


def counter_snapshot():
    """Return a copy safe to serialize while another request records a call."""
    with counters_lock:
        return {scenario: dict(values) for scenario, values in counters.items()}


def call(name, args):
    """The result of a tools/call: add_numbers and check_status answer, restart_service reports a failure."""
    if name == "check_status":
        return {"content": [{"type": "text", "text": "%s is up" % args["service"]}]}
    if name == "restart_service":
        return {"content": [{"type": "text", "text": "restart refused: %s is locked" % args["service"]}], "isError": True}
    return {"content": [{"type": "text", "text": str(args["a"] + args["b"])}]}


class MCP(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def send_json(self, obj, code=200):
        raw = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path == "/_control/counters":
            return self.send_json(counter_snapshot())
        return self.send_json({"error": "not found"}, 404)

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        method, rid = req.get("method"), req.get("id")
        if rid is None:  # a notification
            self.send_response(202)
            self.send_header("Content-Length", "0")
            return self.end_headers()
        if method == "initialize":
            result = {"protocolVersion": req["params"]["protocolVersion"], "capabilities": {"tools": {}},
                      "serverInfo": {"name": "scenario-mcp", "version": "1"}}
        elif method == "tools/list":
            service = {"type": "object", "properties": {"service": {"type": "string"}}, "required": ["service"]}
            result = {"tools": [
                {"name": "add_numbers", "description": "Add two numbers", "inputSchema": {
                    "type": "object", "properties": {"a": {"type": "number"}, "b": {"type": "number"}}, "required": ["a", "b"]}},
                {"name": "check_status", "description": "Check that a service is up", "inputSchema": service},
                {"name": "restart_service", "description": "Restart a service", "inputSchema": service}]}
        elif method == "tools/call":
            name, args = req["params"]["name"], req["params"]["arguments"]
            record_call(self.headers.get("X-Scenario", ""), name)
            result = call(name, args)
        else:
            return self.send_json({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "method not found"}}, 404)
        self.send_json({"jsonrpc": "2.0", "id": rid, "result": result})


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1]) if len(sys.argv) > 1 else 8082), MCP).serve_forever()
