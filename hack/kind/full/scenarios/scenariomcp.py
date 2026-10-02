"""A one-tool MCP server for the full tier's S11 agent: add_numbers(a, b). The agent lists it with
requireApproval, so kagent pauses the task for approval before the tool runs, and courier, on a Binding
that does not allow approvals, denies it. Streamable HTTP, stateless, JSON answers. Test only."""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


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
            result = {"tools": [{"name": "add_numbers", "description": "Add two numbers", "inputSchema": {
                "type": "object", "properties": {"a": {"type": "number"}, "b": {"type": "number"}}, "required": ["a", "b"]}}]}
        else:
            args = req["params"]["arguments"]
            result = {"content": [{"type": "text", "text": str(args["a"] + args["b"])}]}
        self.send_json({"jsonrpc": "2.0", "id": rid, "result": result})


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1]) if len(sys.argv) > 1 else 8082), MCP).serve_forever()
