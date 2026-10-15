"""SSE drain + keep-alive — the main.py half of agent-feed-tools 4.3.

Pins the drain contract the paused-run UX depends on: event/comment items
pass through in order, the channel's `None` closes the drain, and an idle
run gets a protocol-legal keep-alive comment WITHOUT losing an item that
lands at the timeout boundary (the pending get is carried across ticks,
never cancelled mid-wait).
"""

from __future__ import annotations

import asyncio

import pytest

from agent import main as main_module
from agent.types import Event


async def test_drain_forwards_items_in_order_and_ends_on_none():
    queue: asyncio.Queue = asyncio.Queue()
    event = Event(kind="delta", payload={"delta": "hi"})
    await queue.put(("event", event))
    await queue.put(("comment", "from-run"))
    await queue.put(None)

    drained = [item async for item in main_module._drain_run_queue(queue)]
    assert drained == [("event", event), ("comment", "from-run"), None]


async def test_drain_emits_keepalive_after_idle_without_losing_boundary_item(
    monkeypatch,
):
    monkeypatch.setattr(main_module, "_STREAM_KEEPALIVE_SECONDS", 0.05)
    queue: asyncio.Queue = asyncio.Queue()

    agen = main_module._drain_run_queue(queue)
    first = await asyncio.wait_for(agen.__anext__(), timeout=2)
    assert first == ("comment", "keep-alive")

    # An item landing while the drain was idle is delivered next — the
    # carried getter never loses it.
    event = Event(kind="delta", payload={"delta": "late"})
    await queue.put(("event", event))
    second = await asyncio.wait_for(agen.__anext__(), timeout=2)
    assert second == ("event", event)

    await queue.put(None)
    third = await asyncio.wait_for(agen.__anext__(), timeout=2)
    assert third is None
    with pytest.raises(StopAsyncIteration):
        await agen.__anext__()
    await agen.aclose()


async def test_drain_closes_cleanly_when_cancelled_mid_wait(monkeypatch):
    """A client disconnect cancels the drain while its get is pending — the
    generator must finalize without wedging (the pending getter is an
    independent task; the drain cancels it in its finally)."""
    monkeypatch.setattr(main_module, "_STREAM_KEEPALIVE_SECONDS", 5.0)
    queue: asyncio.Queue = asyncio.Queue()
    agen = main_module._drain_run_queue(queue)
    task = asyncio.create_task(agen.__anext__())
    await asyncio.sleep(0.05)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    await agen.aclose()
