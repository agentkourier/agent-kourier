"""Fixtures shared by the tests: the event loop, no reply delay, and a gate to hold a reply mid-stream."""

import asyncio
import types

import pytest

import agent


@pytest.fixture
def anyio_backend():
    return "asyncio"


@pytest.fixture(autouse=True)
def no_delay(monkeypatch):
    monkeypatch.setattr(agent, "CHUNK_DELAY", 0)


@pytest.fixture
def gate(monkeypatch):
    """Holds a reply after each piece until the test sets the gate, so a test can act in the middle of a stream.

    Only agent.py's own use of asyncio is replaced (it calls asyncio.sleep and nothing else): no test waits on a clock.
    """
    opened = asyncio.Event()

    async def hold(_seconds):
        await opened.wait()

    monkeypatch.setattr(agent, "asyncio", types.SimpleNamespace(sleep=hold))
    return opened
