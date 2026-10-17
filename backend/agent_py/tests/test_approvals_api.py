"""Approval endpoint — openspec/changes/agent-feed-tools task 4.4.

Drives the real router over an ASGI transport with fakeredis and a stub
run-metadata layer: the header contract, single-sentinel ownership 404s,
unknown/expired approval 404s, first-decision-wins idempotency, and no
internal error text on the wire.
"""

from __future__ import annotations

import time

import fakeredis.aioredis
import httpx
import pytest
from fastapi import FastAPI

from agent.approvals import DECISION_APPROVE, ApprovalStore
from agent.approvals_api import router
from agent.chat_store import ChatStoreUnavailable


class _StubRunMetadata:
    def __init__(self, owned=()):
        self.owned = set(owned)

    async def get_run(self, run_id, user_id):
        return object() if (run_id, user_id) in self.owned else None


class _StubChatStore:
    def __init__(self, owned=(), unavailable=False):
        self.owned = {(str(sid), uid) for sid, uid in owned}
        self.unavailable = unavailable

    async def get_session(self, session_id, user_id):
        if self.unavailable:
            raise ChatStoreUnavailable("chat store unavailable")
        return object() if (str(session_id), user_id) in self.owned else None


def _app(redis=None, run_metadata=None, chat_store=None) -> FastAPI:
    app = FastAPI()
    app.include_router(router)
    if redis is not None:
        app.state.redis = redis
    if run_metadata is not None:
        app.state.run_metadata = run_metadata
    if chat_store is not None:
        app.state.chat_store = chat_store
    return app


def _client(app: FastAPI) -> httpx.AsyncClient:
    return httpx.AsyncClient(
        transport=httpx.ASGITransport(app=app), base_url="http://test"
    )


@pytest.fixture
async def redis():
    r = fakeredis.aioredis.FakeRedis(decode_responses=True)
    yield r
    await r.aclose()


BODY = {"run_id": "run-1", "approval_id": "appr-1", "decision": "approve"}
HEADERS = {"X-Nyx-User-Id": "7"}


async def _seed_intent(redis, *, approval_id="appr-1", ttl=300):
    await ApprovalStore(redis).put_intent("run-1", approval_id, time.time() + ttl)


async def test_missing_or_non_integer_header_is_400(redis):
    app = _app(redis=redis, run_metadata=_StubRunMetadata({("run-1", 7)}))
    async with _client(app) as client:
        missing = await client.post("/agent/approvals", json=BODY)
        assert missing.status_code == 400
        assert "X-Nyx-User-Id" in missing.json()["detail"]

        bad = await client.post(
            "/agent/approvals", json=BODY, headers={"X-Nyx-User-Id": "abc"}
        )
        assert bad.status_code == 400
        assert "X-Nyx-User-Id" in bad.json()["detail"]


async def test_happy_path_records_first_decision(redis):
    await _seed_intent(redis)
    app = _app(redis=redis, run_metadata=_StubRunMetadata({("run-1", 7)}))
    async with _client(app) as client:
        ok = await client.post("/agent/approvals", json=BODY, headers=HEADERS)
    assert ok.status_code == 200
    assert ok.json() == {"recorded": True}
    assert await ApprovalStore(redis).get_decision("run-1", "appr-1") == DECISION_APPROVE


async def test_first_decision_wins_duplicates_idempotent(redis):
    await _seed_intent(redis)
    app = _app(redis=redis, run_metadata=_StubRunMetadata({("run-1", 7)}))
    async with _client(app) as client:
        first = await client.post("/agent/approvals", json=BODY, headers=HEADERS)
        second = await client.post(
            "/agent/approvals",
            json={**BODY, "decision": "deny"},
            headers=HEADERS,
        )
    assert first.status_code == 200 and first.json() == {"recorded": True}
    assert second.status_code == 200 and second.json() == {"recorded": False}
    assert await ApprovalStore(redis).get_decision("run-1", "appr-1") == DECISION_APPROVE


async def test_non_owned_run_indistinguishable_from_unknown(redis):
    await _seed_intent(redis)
    app = _app(redis=redis, run_metadata=_StubRunMetadata({("run-1", 7)}))
    async with _client(app) as client:
        non_owned = await client.post(
            "/agent/approvals", json=BODY, headers={"X-Nyx-User-Id": "8"}
        )
        unknown = await client.post(
            "/agent/approvals", json={**BODY, "run_id": "nope"}, headers=HEADERS
        )
    assert non_owned.status_code == unknown.status_code == 404
    assert non_owned.json() == unknown.json()
    assert await ApprovalStore(redis).get_decision("run-1", "appr-1") is None


async def test_unknown_and_expired_approval_ids_are_404_never_recorded(redis):
    await _seed_intent(redis, approval_id="expired-1", ttl=-10)
    app = _app(redis=redis, run_metadata=_StubRunMetadata({("run-1", 7)}))
    async with _client(app) as client:
        unknown = await client.post(
            "/agent/approvals", json={**BODY, "approval_id": "ghost"}, headers=HEADERS
        )
        expired = await client.post(
            "/agent/approvals",
            json={**BODY, "approval_id": "expired-1"},
            headers=HEADERS,
        )
    assert unknown.status_code == 404
    assert expired.status_code == 404
    store = ApprovalStore(redis)
    assert await store.get_decision("run-1", "ghost") is None
    assert await store.get_decision("run-1", "expired-1") is None


async def test_store_outage_is_503_without_internal_text():
    class _BrokenRedis:
        async def get(self, *args, **kwargs):
            raise RuntimeError("redis down: secret-ish internals")

    app = _app(redis=_BrokenRedis(), run_metadata=_StubRunMetadata({("run-1", 7)}))
    async with _client(app) as client:
        resp = await client.post("/agent/approvals", json=BODY, headers=HEADERS)
    assert resp.status_code == 503
    assert "redis down" not in resp.text
    assert "RuntimeError" not in resp.text


async def test_no_run_metadata_layer_fails_closed(redis):
    app = _app(redis=redis, run_metadata=None)
    async with _client(app) as client:
        resp = await client.post("/agent/approvals", json=BODY, headers=HEADERS)
    assert resp.status_code == 404
    assert resp.json()["detail"] == "run not found"


async def test_session_owned_decision_recorded_without_runs_row(redis):
    await _seed_intent(redis)
    app = _app(
        redis=redis,
        run_metadata=_StubRunMetadata(),
        chat_store=_StubChatStore({("run-1", 7)}),
    )
    async with _client(app) as client:
        ok = await client.post("/agent/approvals", json=BODY, headers=HEADERS)
    assert ok.status_code == 200
    assert ok.json() == {"recorded": True}
    assert await ApprovalStore(redis).get_decision("run-1", "appr-1") == DECISION_APPROVE


async def test_session_fallback_stays_fail_closed(redis):
    await _seed_intent(redis)
    cases = [
        _StubChatStore(),
        _StubChatStore({("run-1", 8)}),
        _StubChatStore({("run-1", 7)}, unavailable=True),
    ]
    for index, store in enumerate(cases):
        approval_id = f"appr-fallback-{index}"
        await _seed_intent(redis, approval_id=approval_id)
        app = _app(
            redis=redis,
            run_metadata=_StubRunMetadata(),
            chat_store=store,
        )
        async with _client(app) as client:
            resp = await client.post(
                "/agent/approvals",
                json={**BODY, "approval_id": approval_id},
                headers=HEADERS,
            )
        assert resp.status_code == 404, f"case {index}: {resp.status_code}"
        assert resp.json()["detail"] == "run not found"
        assert await ApprovalStore(redis).get_decision("run-1", approval_id) is None
