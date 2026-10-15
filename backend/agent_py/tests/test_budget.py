"""Budget guard tests — fakeredis-backed, OTel span assertions via InMemorySpanExporter."""

from __future__ import annotations

import pytest
from fakeredis import aioredis as fakeaioredis
from opentelemetry import trace
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from agent.budget import BudgetGuard, utc_hour_key


@pytest.fixture
async def redis():
    r = fakeaioredis.FakeRedis(decode_responses=True)
    yield r
    await r.aclose()


class _SpanCollector:
    """Tiny holder so tests can pull finished spans from a fixture."""

    def __init__(self, exporter: InMemorySpanExporter) -> None:
        self._exporter = exporter

    def get_finished_spans(self):
        return self._exporter.get_finished_spans()


@pytest.fixture
def tracer():
    """Returns (tracer, span_collector) tuple."""
    exporter = InMemorySpanExporter()
    provider = TracerProvider(resource=Resource.create({}))
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    return provider.get_tracer("nyx-agent-test"), _SpanCollector(exporter)


def _span_by_name(spans, name):
    return next((s for s in spans if s.name == name), None)


@pytest.mark.asyncio
async def test_check_increment_happy_path(redis, tracer):
    tracer_obj, spans = tracer
    guard = BudgetGuard(redis=redis, cap=1000)
    decision = await guard.check_and_increment(user_id=42, estimated_tokens=100, tracer=tracer_obj)

    assert decision.decision == "allow"
    assert decision.tokens_used == 100
    assert decision.tokens_remaining == 900

    span = _span_by_name(spans.get_finished_spans(), "agent.budget")
    assert span is not None
    attrs = dict(span.attributes)
    assert attrs["user_id"] == 42
    assert attrs["tokens_used"] == 100
    assert attrs["tokens_remaining"] == 900
    assert attrs["decision"] == "allow"


@pytest.mark.asyncio
async def test_check_increment_cap_exceeded(redis, tracer):
    tracer_obj, spans = tracer
    guard = BudgetGuard(redis=redis, cap=1000)
    # Pre-fill the counter at exactly the cap so the next INCRBY would exceed.
    key = utc_hour_key(7)
    await redis.set(key, 1000)

    decision = await guard.check_and_increment(user_id=7, estimated_tokens=50, tracer=tracer_obj)

    assert decision.decision == "exceeded"
    # MEDIUM-10: the INCRBY is rolled back, so tokens_used reflects the
    # pre-check value (1000), not the pre-rollback value (1050).
    assert decision.tokens_used == 1000
    assert decision.tokens_remaining == 0

    span = _span_by_name(spans.get_finished_spans(), "agent.budget")
    assert span.attributes["decision"] == "exceeded"


@pytest.mark.asyncio
async def test_check_increment_rollback_does_not_consume_budget(redis, tracer):
    """A refused call must not consume budget — next call still has full room."""
    tracer_obj, _ = tracer
    guard = BudgetGuard(redis=redis, cap=1000)
    key = utc_hour_key(11)
    await redis.set(key, 990)  # 10 tokens of headroom; estimated=50 will exceed

    # Refused (would have been 1040 without rollback).
    decision = await guard.check_and_increment(user_id=11, estimated_tokens=50, tracer=tracer_obj)
    assert decision.decision == "exceeded"

    # Counter must be back to 990, not 1040.
    after = int(await redis.get(key))
    assert after == 990, f"counter should roll back; got {after}"

    # A subsequent small call within budget is still allowed.
    decision = await guard.check_and_increment(user_id=11, estimated_tokens=5, tracer=tracer_obj)
    assert decision.decision == "allow"
    assert decision.tokens_used == 995


@pytest.mark.asyncio
async def test_check_increment_redis_down(redis, tracer):
    tracer_obj, spans = tracer

    class FailingRedis:
        async def incrby(self, key, amount):
            raise ConnectionError("redis is down")

        async def expire(self, key, ttl):
            raise ConnectionError("redis is down")

    guard = BudgetGuard(redis=FailingRedis(), cap=1000)
    decision = await guard.check_and_increment(user_id=1, estimated_tokens=10, tracer=tracer_obj)

    assert decision.decision == "unavailable"
    assert decision.tokens_used == 0
    assert decision.tokens_remaining == 1000

    span = _span_by_name(spans.get_finished_spans(), "agent.budget")
    assert span.attributes["decision"] == "unavailable"
    # Span event recorded for the error (not in the response wire).
    event_names = [e.name for e in span.events]
    assert "redis_error" in event_names