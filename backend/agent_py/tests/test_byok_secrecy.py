"""Never-log / never-persist guarantee for BYOK keys — task 5.6 of
openspec/changes/add-user-llm-byok (design D11).

A run carrying an override must not leak its plaintext API key into logs,
span attributes, SSE frames, or any persisted row (the production
checkpointer writes every state channel into `langgraph.*`). The key
shape is deliberately non-`sk-` because the Langfuse/OTel scrubber does
not match arbitrary user key shapes — the guarantee is by construction.

The test also asserts the key DID reach the `ChatOpenAI` construction
sites, so a wiring regression that silently stops forwarding the
override cannot make the never-log assertions pass vacuously.

Gated on `TEST_ASYNC_PG_DSN` (self-skips without a live Postgres).
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
from pathlib import Path
from typing import Any, ClassVar

import asyncpg
import fakeredis.aioredis as fakeaioredis
import httpx
import pytest
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")

pytestmark = pytest.mark.skipif(
    not _DSN,
    reason="TEST_ASYNC_PG_DSN not set; needs a live Postgres",
)

_KEY = "e2e-placeholder-key-not-secret"
_FIXTURE = (
    Path(__file__).resolve().parents[3]
    / "frontend"
    / "tests"
    / "e2e"
    / "fixtures"
    / "agent-payload-llm-config.json"
)


async def _drop_schemas() -> None:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        await conn.execute("DROP SCHEMA IF EXISTS langgraph CASCADE")
        await conn.execute("DROP SCHEMA IF EXISTS agent CASCADE")
    finally:
        await conn.close()


class _StubStructuredChain:
    def __init__(self, plan_payload: dict):
        self._payload = plan_payload

    def __or__(self, other):
        return self

    async def ainvoke(self, messages, config=None):
        from agent.types import Plan

        return Plan.model_validate(self._payload)


class _KeyRecordingLlm:
    """ChatOpenAI stand-in that records constructor kwargs (the key path)
    and produces a one-step llm plan on the planner's structured path.

    Implemented as a class so the patched module attributes are callable;
    `bind` returns a chain stub that yields a real `Plan` (matching the
    parser chain the production builder returns).
    """

    instances: ClassVar[list[dict[str, Any]]] = []
    _PLAN: ClassVar[dict[str, Any]] = {
        "steps": [{"id": "step_1", "kind": "llm", "prompt": "answer"}]
    }

    def __init__(self, **kwargs) -> None:
        type(self).instances.append(kwargs)

    @classmethod
    def reset(cls) -> None:
        cls.instances = []

    def bind(self, **kwargs):
        return _StubStructuredChain(self._PLAN)

    async def ainvoke(self, messages, config=None):
        from langchain_core.messages import AIMessage

        return AIMessage(content="stub answer")


def _patch_stubs(monkeypatch, exporter: InMemorySpanExporter) -> None:
    import agent.executor as executor_module
    import agent.main as main_module
    import agent.planner as planner_module
    import agent.rag_client as rag_module
    import agent.tools_client as tools_module

    class _StubRag:
        async def retrieve(self, query):
            from agent.rag_client import RagResult

            return RagResult(context_block="stub context", citations=[])

    class _StubTools:
        async def call(self, name, arguments):
            return {"items": []}

        async def list_tools(self):
            return []

    stub_rag = lambda *a, **kw: _StubRag()
    stub_tools = lambda *a, **kw: _StubTools()

    monkeypatch.setattr(rag_module, "RagClient", stub_rag)
    monkeypatch.setattr(tools_module, "ToolsClient", stub_tools)
    monkeypatch.setattr(main_module, "RagClient", stub_rag)
    monkeypatch.setattr(main_module, "ToolsClient", stub_tools)
    monkeypatch.setattr(planner_module, "ChatOpenAI", _KeyRecordingLlm)
    monkeypatch.setattr(executor_module, "ChatOpenAI", _KeyRecordingLlm)
    monkeypatch.setattr(main_module, "ChatOpenAI", _KeyRecordingLlm)

    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))

    def _setup(settings):
        return provider.get_tracer("nyx-agent-test")

    monkeypatch.setattr(main_module, "setup_observability", _setup)


@pytest.fixture
async def app_ctx(monkeypatch):
    await _drop_schemas()
    _KeyRecordingLlm.reset()

    import agent.config as config_module

    settings = config_module.Settings(
        _env_file=None,
        database_url=_DSN,
        llm_base_url="http://localhost:9999/v1",
        metrics_port=0,
        retention_enabled=False,
        sweep_enabled=False,
        chat_retention_enabled=False,
    )
    monkeypatch.setattr(config_module, "_settings", settings)
    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    import agent.main as main_module

    monkeypatch.setattr(main_module, "get_settings", lambda: settings)

    exporter = InMemorySpanExporter()
    _patch_stubs(monkeypatch, exporter)

    from agent.budget import BudgetGuard
    from agent.main import create_app, lifespan

    app = create_app()
    async with lifespan(app):
        redis = fakeaioredis.FakeRedis(decode_responses=True)
        app.state.budget = BudgetGuard(redis=redis, cap=1_000_000)
        try:
            yield app, exporter
        finally:
            await redis.aclose()


async def _post_chat(app, body: dict, user_id: int = 77) -> httpx.Response:
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test", timeout=30.0
    ) as client:
        return await client.post(
            "/agent/run",
            json=body,
            headers={"X-Nyx-User-Id": str(user_id), "Authorization": "Bearer stub"},
        )


def _first_run_id(sse_text: str) -> str:
    for frame in sse_text.split("\n\n"):
        if frame.startswith("event: run_id"):
            data_line = next(
                line for line in frame.splitlines() if line.startswith("data: ")
            )
            return json.loads(data_line[len("data: ") :])["run_id"]
    raise AssertionError(f"no run_id frame in SSE text: {sse_text[:400]!r}")


async def _drain_title_tasks(timeout: float = 30.0) -> None:
    from agent.main import _TITLE_TASKS

    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while _TITLE_TASKS:
        if loop.time() > deadline:
            raise AssertionError("title-refinement tasks did not drain")
        await asyncio.sleep(0.05)


def _key_reached_a_client() -> bool:
    for kwargs in _KeyRecordingLlm.instances:
        value = kwargs.get("api_key")
        raw = value.get_secret_value() if hasattr(value, "get_secret_value") else value
        if raw == _KEY:
            return True
    return False


def _span_values(span) -> list[Any]:
    attributes = dict(span.attributes or {})
    return [span.name, *attributes.values()]


def _contains_key(value: Any) -> bool:
    if isinstance(value, bytes):
        return _KEY.encode() in value
    return _KEY in str(value)


async def _scan_schemas(needle: str) -> list[str]:
    conn = await asyncpg.connect(dsn=_DSN)
    hits: list[str] = []
    try:
        tables = await conn.fetch(
            """
            SELECT table_schema, table_name
            FROM information_schema.tables
            WHERE table_schema = ANY($1::text[])
            """,
            ["agent", "langgraph"],
        )
        for table in tables:
            qualified = f'"{table["table_schema"]}"."{table["table_name"]}"'
            records = await conn.fetch(f"SELECT * FROM {qualified}")
            for record in records:
                for column, value in record.items():
                    if value is None:
                        continue
                    if isinstance(value, bytes):
                        text = value.decode("utf-8", errors="ignore")
                    else:
                        text = str(value)
                    if needle in text:
                        hits.append(
                            f"{table['table_schema']}.{table['table_name']}.{column}"
                        )
    finally:
        await conn.close()
    return hits


async def _checkpoint_rows(thread_id: str) -> int:
    conn = await asyncpg.connect(dsn=_DSN)
    try:
        row = await conn.fetchrow(
            "SELECT count(*)::int AS n FROM langgraph.checkpoints WHERE thread_id = $1",
            thread_id,
        )
        return row["n"] if row else 0
    finally:
        await conn.close()


async def test_override_key_is_used_but_never_logged_or_persisted(app_ctx, caplog):
    app, exporter = app_ctx
    body = json.loads(_FIXTURE.read_text(encoding="utf-8"))
    assert body["llm_config"]["api_key"] == _KEY

    with caplog.at_level(logging.DEBUG):
        response = await _post_chat(app, body)

    assert response.status_code == 200
    assert "event: terminator" in response.text
    run_id = _first_run_id(response.text)
    await _drain_title_tasks()

    assert _key_reached_a_client(), (
        "the override key never reached a ChatOpenAI constructor — the "
        "never-log assertions would be vacuous"
    )

    assert _KEY not in response.text

    leaked_logs = [
        record.getMessage()
        for record in caplog.records
        if _KEY in record.getMessage()
    ]
    assert leaked_logs == []

    leaked_spans = [
        value
        for span in exporter.get_finished_spans()
        for value in _span_values(span)
        if _contains_key(value)
    ]
    assert leaked_spans == []
    assert exporter.get_finished_spans(), "no spans were recorded — vacuous check"

    leaked_rows = await _scan_schemas(_KEY)
    assert leaked_rows == []

    rows = await _checkpoint_rows(run_id)
    assert rows >= 1, "the run wrote no checkpoints — the persistence check is vacuous"
