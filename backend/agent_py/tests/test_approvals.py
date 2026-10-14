"""Approval decision store — openspec/changes/agent-feed-tools task 4.1.

Store semantics against fakeredis: first-wins `SET NX`, duplicate no-op,
intent → expiry record (the endpoint's live-approval proof), waiter poll
resolving approve/deny/expired/unavailable, and denial-text safety.
"""

from __future__ import annotations

import asyncio
import time

import fakeredis.aioredis
import pytest

from agent import approvals as approvals_module
from agent.approvals import (
    DECISION_APPROVE,
    DECISION_DENY,
    DECISION_EXPIRED,
    DECISION_UNAVAILABLE,
    ApprovalStore,
    denial_text_for,
)


@pytest.fixture
async def store():
    redis = fakeredis.aioredis.FakeRedis(decode_responses=True)
    yield ApprovalStore(redis)
    await redis.aclose()


@pytest.fixture(autouse=True)
def _fast_poll(monkeypatch):
    monkeypatch.setattr(approvals_module, "_POLL_INTERVAL_SECONDS", 0.01)


async def test_intent_roundtrip(store):
    expires = time.time() + 300
    await store.put_intent("r1", "a1", expires)
    assert await store.check_intent("r1", "a1") == pytest.approx(expires)
    assert await store.check_intent("r1", "unknown") is None


async def test_past_expiry_intent_keeps_its_expiry_value(store):
    """A late decision must meet an "intent present but expired" record —
    the endpoint then answers a typed 404 instead of resurrecting the
    approval (D6)."""
    expires = time.time() - 5
    await store.put_intent("r1", "a1", expires)
    assert await store.check_intent("r1", "a1") == pytest.approx(expires)


async def test_first_decision_wins_duplicates_noop(store):
    assert await store.record_decision("r1", "a1", DECISION_APPROVE) is True
    assert await store.record_decision("r1", "a1", DECISION_DENY) is False
    assert await store.get_decision("r1", "a1") == DECISION_APPROVE


async def test_wait_resolves_on_decision(store):
    expires = time.time() + 5
    await store.put_intent("r1", "a1", expires)
    waiter = asyncio.create_task(store.wait_for_decision("r1", "a1", expires))
    # A decision recorded by any replica resolves the wait (spec scenario).
    await store.record_decision("r1", "a1", DECISION_DENY)
    assert await waiter == DECISION_DENY


async def test_wait_expires_without_decision(store):
    assert (
        await store.wait_for_decision("r1", "a1", time.time() + 0.05)
        == DECISION_EXPIRED
    )


async def test_wait_fails_closed_on_store_outage():
    class _BrokenRedis:
        async def get(self, *args, **kwargs):
            raise RuntimeError("redis down")

    store = ApprovalStore(_BrokenRedis())
    assert (
        await store.wait_for_decision("r1", "a1", time.time() + 5)
        == DECISION_UNAVAILABLE
    )


def test_denial_texts_name_tool_and_reason():
    deny = denial_text_for(DECISION_DENY, "delete_feed")
    expired = denial_text_for(DECISION_EXPIRED, "delete_feed")
    unavailable = denial_text_for(DECISION_UNAVAILABLE, "delete_feed")
    assert "delete_feed" in deny and "denied" in deny
    assert "expired" in expired
    assert "unavailable" in unavailable
    for text in (deny, expired, unavailable):
        assert text.startswith("Confirmation required")
        assert "was not executed" in text


async def test_decision_ttl_follows_intent_expiry():
    """Review round 1: a decision recorded with the intent's expiry keeps a
    TTL of remaining window + margin, so a waiter stalled longer than the
    flat margin cannot lose an already-recorded decision."""
    redis = fakeredis.aioredis.FakeRedis(decode_responses=True)
    store = ApprovalStore(redis)
    expires = time.time() + 300
    assert (
        await store.record_decision("r1", "a1", DECISION_APPROVE, expires_at=expires)
        is True
    )
    ttl = await redis.ttl(store.decision_key("r1", "a1"))
    assert ttl > 300  # remaining ≈ 300 plus the margin
    await redis.aclose()
