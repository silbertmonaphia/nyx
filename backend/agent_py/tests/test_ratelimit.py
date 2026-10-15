"""Tests for the per-IP rate limiter + middleware gating /agent/run."""

from __future__ import annotations

import time

import pytest
from httpx import ASGITransport, AsyncClient

from agent.config import Settings
from agent.main import create_app
from agent.ratelimit import IpRateLimiter
from agent.types import ChatMessage, ChatRequest


class _StubStructuredChain:
    """See test_run.py — chainable shim whose `ainvoke` yields the canned Plan."""

    def __init__(self, plan_payload: dict):
        self._payload = plan_payload

    def __or__(self, other):
        return self

    async def ainvoke(self, messages):
        from agent.types import Plan

        return Plan.model_validate(self._payload)


class _StubLlm:
    def __init__(self, *args, **kwargs):
        pass

    async def ainvoke(self, messages):
        from langchain_core.messages import AIMessage

        return AIMessage(content="ok")

    def bind(self, **kwargs):
        # The planner (structured path, default-on) binds `response_format` here.
        return _StubStructuredChain(
            {"steps": [{"id": "step_1", "kind": "llm", "prompt": "ok"}]}
        )


def _settings(burst: int = 200) -> Settings:
    return Settings(
        _env_file=None,  # hermetic: never read the developer's .env
        port=9000,
        metrics_port=9001,
        llm_base_url="http://stub",
        llm_api_key="stub",
        llm_model="stub-model",
        llm_planner_model="",
        plan_max_steps=10,
        step_react_max_iterations=2,
        mcp_url="http://mcp.test",
        rag_bridge_url="http://rag.test",
        redis_url="redis://stub:6379",
        otel_endpoint="http://jaeger:4318",
        otel_service_name="nyx-agent-test",
        requests_per_second=100.0,
        burst=burst,
    )


@pytest.fixture
async def app_low_burst(monkeypatch):
    """App with burst=2 so the rate-limit test can hit it cheaply.

    Patches LLM + Redis clients so the lifespan doesn't try to dial
    a real OpenAI / Redis. The test only cares about HTTP-level
    rate-limiting behavior.
    """
    import langchain_openai
    from fakeredis import aioredis as fakeaioredis

    import agent.executor as executor_module
    import agent.planner as planner_module
    import agent.rag_client as rag_module
    import agent.tools_client as tools_module
    from agent import config as config_module

    settings = _settings(burst=2)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    monkeypatch.setattr(config_module, "_settings", settings)

    class _StubRag:
        async def retrieve(self, query):
            from agent.rag_client import RagResult

            return RagResult(context_block="ctx", citations=[])

    class _StubTools:
        async def call(self, name, arguments):
            return {"items": []}

        async def list_tools(self):
            return []

    rag_module.RagClient = lambda *a, **kw: _StubRag()
    tools_module.ToolsClient = lambda *a, **kw: _StubTools()
    planner_module.ChatOpenAI = _StubLlm
    executor_module.ChatOpenAI = _StubLlm

    from agent import observability as obs_module

    def _noop_setup(s):
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider

        trace.set_tracer_provider(TracerProvider())
        return trace.get_tracer("nyx-agent-test")

    obs_module.setup_observability = _noop_setup

    redis = fakeaioredis.FakeRedis(decode_responses=True)
    try:
        app = create_app(settings)
        async with app.router.lifespan_context(app):
            app.state.redis = redis
            from agent.budget import BudgetGuard

            app.state.budget = BudgetGuard(redis=redis, cap=1000000)
            yield app
    finally:
        planner_module.ChatOpenAI = langchain_openai.ChatOpenAI
        executor_module.ChatOpenAI = langchain_openai.ChatOpenAI
        rag_module.RagClient = rag_module.RagClient.__class__
        tools_module.ToolsClient = tools_module.ToolsClient.__class__
        await redis.aclose()


def test_ip_rate_limiter_burst_then_block():
    limiter = IpRateLimiter(requests_per_second=10.0, burst=3)
    assert limiter.allow("1.2.3.4")
    assert limiter.allow("1.2.3.4")
    assert limiter.allow("1.2.3.4")
    assert not limiter.allow("1.2.3.4"), "burst cap must trip"
    # Different IP unaffected.
    assert limiter.allow("5.6.7.8")
    assert limiter.active_ips() == 2


def test_ip_rate_limiter_window_expires():
    limiter = IpRateLimiter(requests_per_second=10.0, burst=1)
    assert limiter.allow("1.2.3.4")
    assert not limiter.allow("1.2.3.4")
    # Manually age the recorded request beyond the window.
    with limiter._lock:
        q = limiter._visitors["1.2.3.4"]
        q[0] = time.monotonic() - 2.0
    assert limiter.allow("1.2.3.4"), "expired entry must free a slot"


def test_ip_rate_limiter_rejects_bad_config():
    with pytest.raises(ValueError):
        IpRateLimiter(requests_per_second=0, burst=1)
    with pytest.raises(ValueError):
        IpRateLimiter(requests_per_second=1, burst=0)


def test_ip_rate_limiter_evict_idle_drops_stale_entries():
    """Idle visitors must be evicted or the dict grows unbounded
    over distinct source IPs seen during the process lifetime."""
    limiter = IpRateLimiter(requests_per_second=10.0, burst=10)
    # 100 distinct IPs, each doing one request.
    for i in range(100):
        limiter.allow(f"10.0.0.{i}")
    assert limiter.active_ips() == 100
    # Manually age every visitor's last request beyond the eviction
    # threshold so evict_idle drops them all on the next call.
    stale_time = time.monotonic() - 120.0  # > 60s default max_idle_seconds
    with limiter._lock:
        for q in limiter._visitors.values():
            q.append(stale_time)
    evicted = limiter.evict_idle()
    assert evicted == 100
    assert limiter.active_ips() == 0


def test_ip_rate_limiter_evict_idle_keeps_recent_entries():
    limiter = IpRateLimiter(requests_per_second=10.0, burst=10)
    limiter.allow("1.2.3.4")
    limiter.allow("5.6.7.8")
    # Both visitors are recent; evict_idle should drop neither.
    assert limiter.evict_idle() == 0
    assert limiter.active_ips() == 2


def test_ip_rate_limiter_evict_idle_threshold():
    """evict_idle(max_idle_seconds=...) honors the argument."""
    limiter = IpRateLimiter(requests_per_second=10.0, burst=10)
    limiter.allow("1.2.3.4")
    # Age just past a 1s threshold.
    with limiter._lock:
        limiter._visitors["1.2.3.4"].append(time.monotonic() - 1.5)
    # 60s default keeps it; 1s threshold drops it.
    assert limiter.evict_idle(max_idle_seconds=60.0) == 0
    assert limiter.active_ips() == 1
    assert limiter.evict_idle(max_idle_seconds=1.0) == 1
    assert limiter.active_ips() == 0


@pytest.mark.asyncio
async def test_middleware_returns_429_after_burst(app_low_burst):
    """Burst=2 app: third request from the same client IP gets 429."""
    transport = ASGITransport(app=app_low_burst)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        body = ChatRequest(messages=[ChatMessage(role="user", content="hi")]).model_dump()
        headers = {
            "X-Nyx-User-Id": "42",
            "Authorization": "Bearer whatever",
        }
        # First two fit within the burst window — both stream 200.
        r1 = await client.post("/agent/run", json=body, headers=headers)
        r2 = await client.post("/agent/run", json=body, headers=headers)
        assert r1.status_code == 200
        assert r2.status_code == 200
        # Third is over the burst cap — middleware short-circuits with 429.
        r3 = await client.post("/agent/run", json=body, headers=headers)
        assert r3.status_code == 429
        assert "rate limit exceeded" in r3.text


@pytest.mark.asyncio
async def test_middleware_does_not_gate_healthz(app_low_burst):
    """/healthz must NOT be rate-limited — compose scrapes it constantly."""
    transport = ASGITransport(app=app_low_burst)
    async with AsyncClient(transport=transport, base_url="http://test") as client:
        # Burst=2 — fire 5 healthz calls; all should succeed (or 503
        # because Redis is stubbed, but never 429).
        for _ in range(5):
            r = await client.get("/healthz")
            assert r.status_code != 429
