"""Fixtures shared by the tests: the event loop, one test MCP server for the session, and a clock the tests tick."""

import asyncio

import pytest

import agent
from harness import TOKEN, mcp_server


@pytest.fixture
def anyio_backend():
    return "asyncio"


@pytest.fixture(scope="session")
def mcp_process(tmp_path_factory):
    """One test MCP server for the whole run: yields its URL and a function that reads its call log."""
    with mcp_server(tmp_path_factory.mktemp("mcp")) as (url, log):
        yield url, log


class Server:
    """The test MCP server, as one test sees it: `calls()` holds only what reached it since the test began."""

    def __init__(self, url, log) -> None:
        self.url, self.before, self.log = url, len(log()), log

    def calls(self):
        return self.log()[self.before :]


@pytest.fixture
def server(mcp_process, monkeypatch):
    """The test MCP server, and OPS_MCP_TOKEN set to the bearer it requires, as the Deployment's Secret would."""
    monkeypatch.setenv("OPS_MCP_TOKEN", TOKEN)
    return Server(*mcp_process)


class Clock:
    """Stands in for asyncio inside agent.py: the real asyncio, except that sleep waits for tick(), not for time."""

    def __init__(self) -> None:
        self.requested: list[float] = []  # the seconds each sleep asked for
        self.ticks: asyncio.Queue = asyncio.Queue()

    def __getattr__(self, name):
        return getattr(asyncio, name)

    async def sleep(self, seconds):
        self.requested.append(seconds)
        await self.ticks.get()

    def tick(self) -> None:
        """Ends the oldest sleep, as if its time had passed."""
        self.ticks.put_nowait(None)


@pytest.fixture
def clock(monkeypatch):
    """No test waits on a real clock: the heartbeat's sleep ends when the test says so."""
    fake = Clock()
    monkeypatch.setattr(agent, "asyncio", fake)
    return fake
