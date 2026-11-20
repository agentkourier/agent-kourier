"""Tests for the webhook-github recipe's agent (examples/recipes/webhook-github/agent): this image, its agent.yaml, and
github-mcp-server as the sidecar in its pod.

The manifest tests read the recipe's files. The integration tests run the real github-mcp-server binary, pinned in
hack/ci/install-github-mcp-server.sh, against a fake GitHub on loopback, behind the agent with the recipe's agent.yaml
and a scripted model: the tool names, their arguments, the token each request carries, and what reaches GitHub's API.
GITHUB_MCP_SERVER_BIN names the binary; unset, they skip, except under CI=true, where they fail.
"""

import base64
import contextlib
import hashlib
import json
import os
import re
import socket
import socketserver
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import httpx
import pytest
import yaml
from pydantic_ai.messages import RetryPromptPart, ToolReturnPart
from pydantic_ai.models.function import DeltaToolCall, FunctionModel

import agent
from harness import DONE, HERE, artifact_text, free_port, say, serve, wait_for_port

RECIPE = HERE.parent.parent / "webhook-github"
AGENT = RECIPE / "agent"
MANIFESTS = AGENT / "manifests"
PROMPT_AGENT_DEPLOYMENT = HERE / "manifests" / "deployment.yaml"
GITHUB_TOKEN = "github_pat_fake_not_a_secret"  # not a secret: github-mcp-server accepts a token by its prefix alone
TOOLS = ["pull_request_read", "get_file_contents", "pull_request_review_write", "add_issue_comment"]
HEAD, BASE = "0123456789abcdef0123456789abcdef01234567", "89abcdef0123456789abcdef0123456789abcdef"
BINDING = RECIPE / "kourier" / "binding.yaml"
# The id fields of pull request 42 of acme/widgets, as the recipe's ChatConnection maps them from a delivery.
FIELDS = {"number": "42", "repo": "acme/widgets", "headSHA": HEAD, "baseSHA": BASE}


def prompt(trigger: int) -> str:
    """The promptTemplate of the Binding's trigger `trigger` (0: the review, 1: the reply), rendered with FIELDS: the
    prompt the agent gets, before the block of event data that Agent Kourier appends. The templates read only
    {{ .Fields.<name> }}; a name FIELDS lacks, or other braces, fails the test rather than reach the model."""
    template = yaml.safe_load(BINDING.read_text())["spec"]["chat"]["triggers"][trigger]["promptTemplate"]
    rendered = re.sub(r"\{\{ \.Fields\.(\w+) \}\}", lambda field: FIELDS[field.group(1)], template)
    assert "{{" not in rendered and "}}" not in rendered, rendered
    return rendered


def documents(name: str) -> dict:
    return yaml.safe_load((MANIFESTS / name).read_text())


def pod() -> dict:
    return documents("deployment.yaml")["spec"]["template"]["spec"]


def containers() -> dict:
    return {c["name"]: c for c in pod()["containers"]}


def mcp_entry() -> dict:
    return yaml.safe_load((AGENT / "agent.yaml").read_text())["capabilities"][0]["MCP"]


def sidecar_flags() -> dict:
    """The sidecar's --name=value arguments, after its subcommand."""
    command, *flags = containers()["github-mcp-server"]["args"]
    assert command == "http"
    return dict(flag.removeprefix("--").split("=", 1) for flag in flags)


def test_the_recipe_agent_yaml_passes_the_policy_with_the_spec_model(monkeypatch):
    """The pod's own start: the spec's model is built (from a fake key) and checked, and ask_user is off."""
    monkeypatch.setenv("GITHUB_TOKEN", GITHUB_TOKEN)
    monkeypatch.setenv("ANTHROPIC_API_KEY", "fake-provider-key")
    loaded, secrets = agent.load_agent(str(AGENT / "agent.yaml"), asks=agent.asks_user("0"))
    assert loaded.name == "github-review-agent" and secrets == {GITHUB_TOKEN}
    assert set(loaded.toolsets[0].tools) == set(), "no function tool: ask_user is off"


def test_the_agent_may_call_exactly_the_four_tools_and_the_sidecar_registers_the_same_four():
    assert mcp_entry()["allowed_tools"] == TOOLS
    assert sidecar_flags()["tools"].split(",") == TOOLS
    assert mcp_entry()["url"] == f"http://127.0.0.1:{sidecar_flags()['port']}/"
    assert sidecar_flags()["listen-host"] == "127.0.0.1", "only the agent in the pod reaches the sidecar"
    assert "{{" not in yaml.safe_load((AGENT / "agent.yaml").read_text())["instructions"]


def test_the_configmap_holds_the_recipe_agent_yaml():
    assert documents("configmap.yaml")["data"]["agent.yaml"] == (AGENT / "agent.yaml").read_text()


def test_the_deployment_runs_the_prompt_agent_image_without_ask_user_and_the_token_only_in_the_agent():
    agent_container = containers()["github-review-agent"]
    prompt_agent = yaml.safe_load(PROMPT_AGENT_DEPLOYMENT.read_text())["spec"]["template"]["spec"]["containers"][0]
    assert agent_container["image"] == prompt_agent["image"], "the image the prompt-agent recipe names"
    env = {e["name"]: e for e in agent_container["env"]}
    assert env["PROMPT_AGENT_ASK_USER"]["value"] == "0"
    assert env["PROMPT_AGENT_SPEC"]["value"] == "/etc/prompt-agent/agent.yaml"
    assert env["GITHUB_TOKEN"]["valueFrom"]["secretKeyRef"] == {"name": "github-review-agent-github", "key": "token"}
    assert agent.ENV.findall(mcp_entry()["authorization_token"]) == ["GITHUB_TOKEN"]
    assert "PROMPT_AGENT_ALLOW_ANONYMOUS" not in env, "the agent requires the Binding's token"
    token_file = Path(env["PROMPT_AGENT_TOKEN_FILE"]["value"])
    mount = next(m for m in agent_container["volumeMounts"] if m["mountPath"] == str(token_file.parent))
    volume = next(v for v in pod()["volumes"] if v["name"] == mount["name"])
    assert volume["secret"]["secretName"] == "github-review-agent-bearer" and token_file.name == "token"
    binding = yaml.safe_load((RECIPE / "kourier" / "binding.yaml").read_text())
    assert binding["spec"]["identity"]["tokenSecretRef"] == {"name": "github-review-agent-bearer"}


def test_the_sidecar_is_pinned_by_digest_holds_no_secret_and_runs_locked_down():
    sidecar = containers()["github-mcp-server"]
    image, _, digest = sidecar["image"].partition("@sha256:")
    assert image.startswith("ghcr.io/github/github-mcp-server:v") and len(digest) == 64
    held = json.dumps(sidecar)
    assert "secret" not in held.lower() and "env" not in sidecar and "volumeMounts" not in sidecar
    assert sidecar["securityContext"] == {
        "allowPrivilegeEscalation": False,
        "readOnlyRootFilesystem": True,
        "capabilities": {"drop": ["ALL"]},
    }
    assert pod()["securityContext"]["runAsNonRoot"] is True, "the image's own user is root"
    assert sidecar["resources"]["limits"]["memory"]


def test_the_service_serves_a2a_alone_and_only_the_broker_pods_may_connect():
    assert [p["port"] for p in documents("service.yaml")["spec"]["ports"]] == [8080]
    policy = documents("networkpolicy.yaml")
    [rule] = policy["spec"]["ingress"]
    [peer] = rule["from"]
    assert peer["namespaceSelector"]["matchLabels"] == {"kubernetes.io/metadata.name": "agent-kourier"}
    assert peer["podSelector"]["matchLabels"] == {"app.kubernetes.io/name": "agent-kourier"}
    assert rule["ports"] == [{"protocol": "TCP", "port": 8080}]
    backend = yaml.safe_load((RECIPE / "kourier" / "agentbackend.yaml").read_text())
    assert backend["spec"]["url"] == "http://github-review-agent.github-agent.svc.cluster.local:8080"


# The integration tests.

WIDGET = b"package widget\n"
WIDGET_SHA = hashlib.sha1(b"blob %d\0" % len(WIDGET) + WIDGET).hexdigest()  # noqa: S324 (git's blob id; the server checks it)
PULL = {
    "number": 42,
    "state": "open",
    "title": "Add a widget",
    "body": "Adds one widget.",
    "html_url": "https://github.example/acme/widgets/pull/42",
    "user": {"login": "octocat"},
    "head": {"sha": HEAD, "ref": "widget"},
    "base": {"sha": BASE, "ref": "main"},
}
FILE = {"type": "file", "name": "widget.go", "path": "widget.go", "sha": WIDGET_SHA, "size": len(WIDGET)}
REST = {  # what the fake answers under /api/v3; anything else is 404
    ("GET", "/repos/acme/widgets/pulls/42"): (200, PULL),
    ("GET", "/repos/acme/widgets/pulls/42/files"): (200, [{"filename": "widget.go", "status": "added", "changes": 1}]),
    ("GET", "/repos/acme/widgets/contents/widget.go"): (
        200,
        {**FILE, "encoding": "base64", "content": base64.b64encode(WIDGET).decode()},
    ),
    ("GET", "/repos/acme/widgets/issues/42/comments"): (200, [{"id": 7, "body": "@acme-review-agent why?"}]),
    ("POST", "/repos/acme/widgets/issues/42/comments"): (201, {"id": 8}),
}


class FakeGitHub(ThreadingHTTPServer):
    """GitHub's REST API under /api/v3 and its GraphQL API at /api/graphql, where github-mcp-server calls a GitHub
    Enterprise Server host (--gh-host): pull request 42 of acme/widgets, and a record of every request."""

    def __init__(self) -> None:
        super().__init__(("127.0.0.1", 0), FakeGitHubHandler)
        self.requests: list[dict] = []

    def server_bind(self) -> None:  # HTTPServer's own looks up the host's name, which can wait on reverse DNS
        socketserver.TCPServer.server_bind(self)

    def where(self) -> list[tuple[str, str]]:
        return [(r["method"], r["path"].split("?", 1)[0]) for r in self.requests]


class FakeGitHubHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def answer(self) -> None:
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        body = json.loads(raw) if raw else None
        self.server.requests.append(
            {"method": self.command, "path": self.path, "authorization": self.headers["Authorization"], "body": body}
        )
        path = self.path.split("?", 1)[0]
        if (self.command, path) == ("POST", "/api/graphql"):  # the review: the pull request's node ID, then the review
            if "addPullRequestReview" in body["query"]:
                status, answer = 200, {"data": {"addPullRequestReview": {"pullRequestReview": {"id": "PRR_1"}}}}
            else:
                status, answer = 200, {"data": {"repository": {"pullRequest": {"id": "PR_42"}}}}
        else:
            status, answer = REST.get((self.command, path.removeprefix("/api/v3")), (404, {"message": "Not Found"}))
        data = json.dumps(answer).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    do_GET = do_POST = answer

    def log_message(self, *args) -> None:
        pass


class Recorder:
    """A TCP forwarder between the agent and github-mcp-server that keeps every byte the agent sends, so a test can
    read the MCP requests' headers: the protocol version, and the Mcp-Param-* headers the server checks."""

    def __init__(self, target: int) -> None:
        self.target, self.sent, self.server = target, bytearray(), socket.create_server(("127.0.0.1", 0))
        self.port = self.server.getsockname()[1]
        threading.Thread(target=self.accept, daemon=True).start()

    def accept(self) -> None:
        with contextlib.suppress(OSError):  # the server socket closed: the test is over
            while True:
                agent_side, _ = self.server.accept()
                server_side = socket.create_connection(("127.0.0.1", self.target))
                threading.Thread(target=self.pipe, args=(agent_side, server_side, True), daemon=True).start()
                threading.Thread(target=self.pipe, args=(server_side, agent_side, False), daemon=True).start()

    def pipe(self, source: socket.socket, sink: socket.socket, keep: bool) -> None:
        with contextlib.suppress(OSError), source, sink:
            while data := source.recv(65536):
                if keep:
                    self.sent += data
                sink.sendall(data)

    def headers(self) -> str:
        """What the agent sent, lowercased: header names are case-insensitive."""
        return self.sent.decode(errors="replace").lower()


def binary() -> str:
    path = os.environ.get("GITHUB_MCP_SERVER_BIN", "")
    if not path:
        if os.environ.get("CI") == "true":
            pytest.fail("GITHUB_MCP_SERVER_BIN is not set: CI runs these tests (hack/ci/install-github-mcp-server.sh)")
        pytest.skip("GITHUB_MCP_SERVER_BIN is not set (hack/ci/install-github-mcp-server.sh fetches the binary)")
    return path


@pytest.fixture
def github(tmp_path):
    """The fake GitHub, github-mcp-server in front of it with the sidecar's flags, and a Recorder in front of that."""
    path, port, log_path = binary(), free_port(), tmp_path / "github-mcp-server.log"
    with contextlib.ExitStack() as stack:
        fake = FakeGitHub()
        stack.callback(fake.server_close)
        threading.Thread(target=fake.serve_forever, daemon=True).start()
        stack.callback(fake.shutdown)
        flags = {**sidecar_flags(), "port": str(port), "gh-host": f"http://127.0.0.1:{fake.server_address[1]}"}
        # PATH alone: the Deployment gives the sidecar no variables, so none of the runner's (a GITHUB_* one) may
        # change what it does.
        env = {"PATH": os.environ.get("PATH", "")}
        with log_path.open("w") as log:
            command = [path, "http", *(f"--{k}={v}" for k, v in flags.items())]
            proc = subprocess.Popen(command, stdout=log, stderr=log, env=env)  # noqa: S603
        stack.callback(proc.wait)
        stack.callback(proc.terminate)
        wait_for_port(port)
        try:
            socket.create_connection(("127.0.0.1", port), 1).close()
        except OSError as err:
            pytest.fail(
                f"github-mcp-server does not listen on {port} ({err}; exit code {proc.poll()}). Its log:\n"
                + log_path.read_text()
            )
        recorder = Recorder(port)
        stack.callback(recorder.server.close)
        yield fake, recorder, f"http://127.0.0.1:{recorder.port}/"


@pytest.fixture
def spec(tmp_path, github, monkeypatch):
    """The recipe's agent.yaml with the sidecar's URL replaced by the test's, and the token the pod gets."""
    monkeypatch.setenv("GITHUB_TOKEN", GITHUB_TOKEN)
    data = yaml.safe_load((AGENT / "agent.yaml").read_text())
    data["capabilities"][0]["MCP"]["url"] = github[2]
    path = tmp_path / "agent.yaml"
    path.write_text(yaml.safe_dump(data))
    return str(path)


def scripted(calls: list[tuple[str, dict]], results: list):
    """A model that makes `calls` in turn, one per response, then says it is done. `results` gets each tool's result:
    a ToolReturnPart, or a RetryPromptPart for a call that failed."""

    async def stream(messages, info):
        last = messages[-1].parts[-1]
        if isinstance(last, ToolReturnPart | RetryPromptPart):
            results.append(last)
        if len(results) < len(calls):
            name, args = calls[len(results)]
            yield {0: DeltaToolCall(name=name, json_args=json.dumps(args))}
        else:
            yield "Posted."

    return FunctionModel(stream_function=stream)


PR = {"owner": "acme", "repo": "widgets", "pullNumber": 42}


@pytest.mark.anyio
@pytest.mark.parametrize("event", ["COMMENT", "REQUEST_CHANGES"])
async def test_a_review_reads_the_pull_request_and_posts_one_review_with_the_token(github, spec, event):
    """The review flow of agent.yaml's instructions, with the arguments it names, reaches GitHub as it should: the
    review is one GraphQL addPullRequestReview on the head commit, of the event the model chose (COMMENT, or
    REQUEST_CHANGES for a defect), and every request has the token."""
    fake, recorder, _ = github
    results = []
    calls = [
        ("pull_request_read", {"method": "get", **PR}),
        ("pull_request_read", {"method": "get_files", **PR}),
        ("get_file_contents", {"owner": "acme", "repo": "widgets", "path": "widget.go", "ref": HEAD}),
        (
            "pull_request_review_write",
            {"method": "create", "event": event, "commitID": HEAD, "body": "Fine.", **PR},
        ),
    ]
    async with serve(spec, scripted(calls, results), ask_user="0") as client:
        _, task = await say(client, prompt(0))
    assert task.status.state == DONE and artifact_text(task) == "Posted."
    assert all(isinstance(r, ToolReturnPart) for r in results), results
    assert results[2].content and "package widget" in json.dumps(results[2].content), "the file's text"
    assert fake.where()[:3] == [
        ("GET", "/api/v3/repos/acme/widgets/pulls/42"),
        ("GET", "/api/v3/repos/acme/widgets/pulls/42/files"),
        ("GET", "/api/v3/repos/acme/widgets/contents/widget.go"),
    ]
    assert f"ref={HEAD}" in fake.requests[2]["path"], "the file at the head commit"
    [mutation] = [r for r in fake.requests if r["body"] and "addPullRequestReview" in r["body"]["query"]]
    review = mutation["body"]["variables"]["input"]
    assert review == {"pullRequestId": "PR_42", "commitOID": HEAD, "event": event, "body": "Fine."}
    assert {r["authorization"] for r in fake.requests} == {f"Bearer {GITHUB_TOKEN}"}, "every request, the token"
    # MCP 2026-07-28: the server answers 400 to a tools/call whose Mcp-Param-* headers are missing or do not match the
    # body's owner and repo. The pinned client negotiates that version and sends them.
    assert "mcp-protocol-version: 2026-07-28" in recorder.headers()
    assert "mcp-param-owner: acme" in recorder.headers() and "mcp-param-repo: widgets" in recorder.headers()


@pytest.mark.anyio
async def test_a_reply_reads_the_comments_and_posts_one_issue_comment(github, spec):
    fake, _, _ = github
    results = []
    calls = [
        ("pull_request_read", {"method": "get", **PR}),
        ("pull_request_read", {"method": "get_comments", **PR}),
        ("add_issue_comment", {"owner": "acme", "repo": "widgets", "issue_number": 42, "body": "Because."}),
    ]
    async with serve(spec, scripted(calls, results), ask_user="0") as client:
        _, task = await say(client, prompt(1))
    assert task.status.state == DONE and all(isinstance(r, ToolReturnPart) for r in results), results
    assert fake.where() == [
        ("GET", "/api/v3/repos/acme/widgets/pulls/42"),
        ("GET", "/api/v3/repos/acme/widgets/issues/42/comments"),
        ("POST", "/api/v3/repos/acme/widgets/issues/42/comments"),
    ]
    assert fake.requests[2]["body"] == {"body": "Because."}
    assert {r["authorization"] for r in fake.requests} == {f"Bearer {GITHUB_TOKEN}"}


@pytest.mark.anyio
async def test_the_sidecar_serves_exactly_the_four_tools(github):
    _, _, url = github
    headers = {"Authorization": f"Bearer {GITHUB_TOKEN}", "Accept": "application/json, text/event-stream"}
    async with httpx.AsyncClient() as http:
        response = await http.post(url, headers=headers, json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
    data = json.loads(next(line for line in response.text.splitlines() if line.startswith("data: "))[6:])
    assert sorted(t["name"] for t in data["result"]["tools"]) == sorted(TOOLS)
