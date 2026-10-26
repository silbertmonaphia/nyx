"""Live smoke: Langfuse tracing state and export failures are visible by default.

openspec/changes/agent-langfuse-log-visibility, task 1.2. The unit tests in
`tests/test_langfuse_tracing.py` pin the logging configuration; this file
boots the real app (`create_app` + lifespan — the same startup path
`python -m agent.main` runs), drives one run through the SSE endpoint with
tracing pointed at a host that cannot be reached, and asserts what an
operator sees in the process log without setting `LANGFUSE_DEBUG` or any
other flag:

  - the startup INFO lines render — the Langfuse one names the configured
    host, the runtime `nyx.agent` one names the metrics port;
  - the first WARN-or-above export diagnostic arrives within a bounded wait;
  - tracing disabled emits no Langfuse line at all;
  - a real `uvicorn` boot (the dev entrypoint and the compose CMD) emits the
    startup lines once and does not duplicate uvicorn's own lines.

`_UNRESOLVABLE_HOST` is the host used where the diagnostic must *name* the
host: a name-resolution failure returns fast enough for the exporter's retry
WARNING — whose text carries the exception, host included — to render.
`_REFUSED_HOST` pins the coarser property for the connect-timeout class:
the transport burns the exporter's whole retry deadline before failing, so
the surfaced ERROR reads "failed to export spans batch due to timeout, max
retries or shutdown" and the startup line stays the operator's host anchor.
The host-naming assertion is skipped when the name resolves anyway (a
wildcard-DNS resolver answers for `.invalid` too); the coarse
diagnostic-arrived assertion still runs.
"""

from __future__ import annotations

import json
import logging
import os
import re
import socket
import subprocess
import sys
import threading
import time
import uuid
from collections.abc import Iterator
from contextlib import asynccontextmanager, contextmanager
from pathlib import Path
from typing import Any, ClassVar
from urllib.parse import urlparse

import httpx
import pytest
from fakeredis import aioredis as fakeaioredis
from langchain_core.language_models.chat_models import BaseChatModel
from langchain_core.messages import AIMessage
from langchain_core.outputs import ChatGeneration, ChatResult

from agent import observability as obs_module
from agent.budget import BudgetGuard
from agent.config import Settings
from agent.main import create_app
from agent.types import ChatMessage, ChatRequest
from tests.service_auth_fixtures import SERVICE_AUTH_HEADERS

AGENT_PY_DIR = Path(__file__).resolve().parents[1]

_UNRESOLVABLE_HOST = "http://nyx-langfuse-unreachable.invalid:3000"
_REFUSED_HOST = "http://127.0.0.1:9"
_EXPORT_DIAGNOSTIC_TIMEOUT_SECONDS = 30.0
_BOOT_TIMEOUT_SECONDS = 60.0

_STARTUP_LINE = (
    r"^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3} INFO agent\.observability: "
    r"langfuse tracing enabled \(host=%s\)$"
)

_PLAN: dict[str, Any] = {
    "steps": [
        {"id": "step_1", "kind": "tool", "tool": "list_feeds", "args": {}},
        {"id": "step_2", "kind": "llm", "prompt": "summarise"},
    ]
}

_USAGE = {"input_tokens": 11, "output_tokens": 22, "total_tokens": 33}


class _FakeChatModel(BaseChatModel):
    """Real langchain BaseChatModel (not a duck-type): the Langfuse callback
    handler only fires through the langchain callback machinery, and the
    generations it captures are what lands in the export batch this smoke
    watches fail."""

    model_config: ClassVar[dict[str, Any]] = {"arbitrary_types_allowed": True, "extra": "allow"}

    call_count: ClassVar[int] = 0

    @property
    def _llm_type(self) -> str:
        return "fake-langfuse-visibility"

    def _generate(self, messages, stop=None, run_manager=None, **kwargs) -> ChatResult:
        type(self).call_count += 1
        content = (
            json.dumps(_PLAN) if type(self).call_count == 1 else "summary: no feeds"
        )
        message = AIMessage(content=content, usage_metadata=dict(_USAGE))
        return ChatResult(generations=[ChatGeneration(message=message)])

    def bind_tools(self, tools, **kwargs):
        return self.bind(tools=tools, **kwargs)


class _RecordCapture(logging.Handler):
    """Collect every record that reaches root, whatever its level."""

    def __init__(self) -> None:
        super().__init__(level=logging.DEBUG)
        self.records: list[logging.LogRecord] = []

    def emit(self, record: logging.LogRecord) -> None:
        self.records.append(record)


@contextmanager
def _capture_records() -> Iterator[list[logging.LogRecord]]:
    handler = _RecordCapture()
    root = logging.getLogger()
    root.addHandler(handler)
    try:
        yield handler.records
    finally:
        root.removeHandler(handler)


def _settings(**overrides: Any) -> Settings:
    suffix = uuid.uuid4().hex[:8]
    base: dict[str, Any] = {
        "_env_file": None,
        "port": 9000,
        "metrics_port": 9004,
        "llm_base_url": "http://stub",
        "llm_api_key": "stub",
        "llm_model": "stub-model",
        "llm_planner_model": "",
        "plan_max_steps": 10,
        "step_react_max_iterations": 2,
        "mcp_url": "http://mcp.test",
        "rag_bridge_url": "http://rag.test",
        "redis_url": "redis://stub:6379",
        "otel_endpoint": "http://jaeger:4318",
        "otel_service_name": "nyx-agent-test",
        "database_url": "",
        "langfuse_enabled": True,
        "langfuse_public_key": f"pk-lf-visibility-{suffix}",
        "langfuse_secret_key": f"sk-lf-visibility-{suffix}",
        "langfuse_host": _UNRESOLVABLE_HOST,
    }
    base.update(overrides)
    return Settings(**base)


def _clear_sdk_registry() -> None:
    try:
        from langfuse._client.resource_manager import LangfuseResourceManager

        LangfuseResourceManager._instances.clear()
    except Exception:  # pragma: no cover - SDK internals moved  # noqa: BLE001, S110
        pass


@asynccontextmanager
async def _boot_app(
    monkeypatch: pytest.MonkeyPatch,
    *,
    langfuse_host: str = _UNRESOLVABLE_HOST,
    langfuse_enabled: bool = True,
) -> Any:
    """Boot the app the way the process does, with NO span exporter injected:
    the lifespan builds the real OTLP exporter against `langfuse_host`."""

    settings = _settings(
        langfuse_enabled=langfuse_enabled, langfuse_host=langfuse_host
    )
    redis = fakeaioredis.FakeRedis(decode_responses=True)

    import agent.config as config_module

    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    monkeypatch.setattr(config_module, "_settings", settings)

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
            return {"items": [1, 2, 3]}

        async def list_tools(self):
            return [
                {
                    "type": "function",
                    "annotations": {"readOnlyHint": True},
                    "function": {
                        "name": "list_feeds",
                        "description": "List user's feeds",
                        "parameters": {"type": "object", "properties": {}},
                    },
                }
            ]

    for module in (rag_module, main_module):
        monkeypatch.setattr(module, "RagClient", lambda *a, **kw: _StubRag())
    for module in (tools_module, main_module):
        monkeypatch.setattr(module, "ToolsClient", lambda *a, **kw: _StubTools())

    _FakeChatModel.call_count = 0
    for module in (planner_module, executor_module):
        monkeypatch.setattr(module, "ChatOpenAI", _FakeChatModel)

    def _no_jaeger(settings):
        from opentelemetry import trace as otel_trace
        from opentelemetry.sdk.trace import TracerProvider

        provider = TracerProvider()
        otel_trace.set_tracer_provider(provider)
        return provider.get_tracer("nyx-agent-visibility-test")

    for module in (obs_module, main_module):
        monkeypatch.setattr(module, "setup_observability", _no_jaeger)

    app = create_app(settings)
    try:
        async with app.router.lifespan_context(app):
            app.state.redis = redis
            app.state.budget = BudgetGuard(redis=redis, cap=1000000)
            yield app
    finally:
        obs_module.shutdown_langfuse()
        _clear_sdk_registry()
        await redis.aclose()


async def _post_chat(app: Any, body: dict) -> httpx.Response:
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport, base_url="http://test", headers=SERVICE_AUTH_HEADERS
    ) as client:
        return await client.post(
            "/agent/run",
            json=body,
            headers={
                "X-Nyx-User-Id": "42",
                "Authorization": "Bearer stub-token-not-verified",
            },
        )


def _chat_body() -> dict:
    return ChatRequest(messages=[ChatMessage(role="user", content="hi")]).model_dump()


def _langfuse_lines(records: list[logging.LogRecord]) -> list[logging.LogRecord]:
    return [
        r
        for r in records
        if "langfuse" in r.getMessage().lower() or "langfuse" in r.name.lower()
    ]


def _runtime_lines(records: list[logging.LogRecord]) -> list[logging.LogRecord]:
    return [
        r
        for r in records
        if r.name == "nyx.agent"
        and "prometheus exporter listening" in r.getMessage()
    ]


def _name_resolves(hostname: str, timeout: float = 2.0) -> bool:
    """True when the name resolves — or when the probe cannot rule it out.

    A wildcard-DNS resolver answers for `.invalid` names too, so the
    host-naming assertion only runs when resolution positively fails.
    """
    result: list[bool] = []

    def _probe() -> None:
        try:
            socket.getaddrinfo(hostname, None)
            result.append(True)
        except OSError:
            result.append(False)

    probe = threading.Thread(target=_probe, daemon=True)
    probe.start()
    probe.join(timeout)
    return result[0] if result else True


def _export_diagnostics(records: list[logging.LogRecord]) -> list[logging.LogRecord]:
    return [
        r
        for r in records
        if r.levelno >= logging.WARNING
        and r.name.startswith("opentelemetry.exporter")
    ]


def _wait_for_export_diagnostic(
    records: list[logging.LogRecord],
    timeout: float = _EXPORT_DIAGNOSTIC_TIMEOUT_SECONDS,
) -> logging.LogRecord | None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        found = _export_diagnostics(records)
        if found:
            return found[0]
        time.sleep(0.1)
    return None


@pytest.mark.parametrize(
    "host, diagnostic_names_host",
    [(_UNRESOLVABLE_HOST, True), (_REFUSED_HOST, False)],
)
async def test_export_failure_is_visible_after_one_run(
    monkeypatch: pytest.MonkeyPatch, host: str, diagnostic_names_host: bool
) -> None:
    """One run against an unreachable Langfuse, then the operator-visible trail:
    startup INFO naming the host + a WARN-or-above export diagnostic within a
    bounded wait, with tracing enabled by configuration only."""
    with _capture_records() as records:
        async with _boot_app(monkeypatch, langfuse_host=host) as app:
            resp = await _post_chat(app, _chat_body())
            assert resp.status_code == 200
            assert "event: done\n" in resp.text, (
                "the run must complete: export failures never touch the SSE "
                f"contract; stream was {resp.text[:400]!r}"
            )

            startup = [
                r
                for r in records
                if r.name == "agent.observability"
                and "langfuse tracing enabled" in r.getMessage()
            ]
            assert len(startup) == 1, [
                (r.levelno, r.name, r.getMessage()) for r in records
            ]
            assert startup[0].levelno == logging.INFO
            assert host in startup[0].getMessage()

            runtime = _runtime_lines(records)
            assert len(runtime) == 1, [
                (r.levelno, r.name, r.getMessage()) for r in records
            ]
            assert runtime[0].levelno == logging.INFO

            diagnostic = _wait_for_export_diagnostic(records)
            assert diagnostic is not None, (
                "no WARN-or-above export diagnostic within "
                f"{_EXPORT_DIAGNOSTIC_TIMEOUT_SECONDS:.0f}s; records were "
                f"{[(r.levelno, r.name, r.getMessage()[:120]) for r in records]}"
            )
            hostname = urlparse(host).hostname
            if (
                diagnostic_names_host
                and hostname is not None
                and not _name_resolves(hostname)
            ):
                assert hostname in diagnostic.getMessage(), (
                    "the export diagnostic must let the operator identify the "
                    f"host; got {diagnostic.getMessage()!r}"
                )
        assert _export_diagnostics(records), (
            "the diagnostic must not disappear when the app shuts down"
        )


async def test_disabled_tracing_emits_no_langfuse_line(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Tracing off stays silent: no startup line, no export diagnostics, no
    Langfuse-named record at any level across the run and the shutdown."""
    with _capture_records() as records:
        async with _boot_app(monkeypatch, langfuse_enabled=False) as app:
            resp = await _post_chat(app, _chat_body())
            assert resp.status_code == 200
            assert "event: done\n" in resp.text

    assert _langfuse_lines(records) == [], [
        (r.levelno, r.name, r.getMessage()) for r in _langfuse_lines(records)
    ]
    assert _export_diagnostics(records) == []
    runtime = _runtime_lines(records)
    assert len(runtime) == 1, [(r.levelno, r.name, r.getMessage()) for r in records]
    assert runtime[0].levelno == logging.INFO


def _free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def _boot_lines(argv: list[str], env: dict[str, str]) -> list[str]:
    """Run the agent process, collect its merged output until uvicorn reports
    the server up, then stop it. Returns the lines seen."""
    proc = subprocess.Popen(
        [sys.executable, *argv],
        cwd=str(AGENT_PY_DIR),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    lines: list[str] = []

    def _read() -> None:
        assert proc.stdout is not None
        lines.extend(line.rstrip("\n") for line in proc.stdout)

    reader = threading.Thread(target=_read, daemon=True)
    reader.start()

    def _seen(needle: str) -> bool:
        return any(needle in line for line in lines)

    deadline = time.monotonic() + _BOOT_TIMEOUT_SECONDS
    try:
        while time.monotonic() < deadline:
            if _seen("Uvicorn running on") and _seen("langfuse tracing enabled"):
                break
            if proc.poll() is not None:
                break
            time.sleep(0.1)
        else:
            pytest.fail(
                "agent boot did not reach 'Uvicorn running on' within "
                f"{_BOOT_TIMEOUT_SECONDS:.0f}s; output was {lines}"
            )
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:  # pragma: no cover - wedged shutdown
            proc.kill()
            proc.wait(timeout=15)
        reader.join(timeout=5)
    if proc.returncode not in (0, -15, 143):
        pytest.fail(f"agent process exited {proc.returncode}; output was {lines}")
    return lines


@pytest.mark.parametrize(
    "base_argv, binds_via_argv",
    [
        pytest.param(["-m", "agent.main"], False, id="dev-entrypoint"),
        pytest.param(
            ["-m", "uvicorn", "agent.main:app", "--log-level", "info"],
            True,
            id="compose-cmd",
        ),
    ],
)
def test_real_boot_formats_the_startup_line_once(
    base_argv: list[str], binds_via_argv: bool
) -> None:
    """A real uvicorn boot: the startup INFO renders with the process format
    exactly once, and the root handler does not duplicate uvicorn's own lines
    (they carry propagate=False and stay on uvicorn's handlers)."""
    port = _free_port()
    metrics_port = _free_port()
    host = _REFUSED_HOST
    env = {k: v for k, v in os.environ.items() if not k.startswith("PYTHON_AGENT_")}
    env.update(
        PYTHONPATH=str(AGENT_PY_DIR),
        PYTHON_AGENT_PORT=str(port),
        PYTHON_AGENT_METRICS_PORT=str(metrics_port),
        PYTHON_AGENT_LANGFUSE_ENABLED="true",
        PYTHON_AGENT_LANGFUSE_HOST=host,
        PYTHON_AGENT_LANGFUSE_PUBLIC_KEY="pk-lf-visibility-smoke",
        PYTHON_AGENT_LANGFUSE_SECRET_KEY="sk-lf-visibility-smoke",
        PYTHON_AGENT_LLM_BASE_URL="http://127.0.0.1:9/v1",
        PYTHON_AGENT_REDIS_URL="redis://127.0.0.1:9",
        PYTHON_AGENT_MCP_URL="http://127.0.0.1:9/mcp",
        PYTHON_AGENT_RAG_BRIDGE_URL="http://127.0.0.1:9/internal/rag/retrieve",
    )
    startup_pattern = re.compile(_STARTUP_LINE % re.escape(host))

    argv = list(base_argv)
    if binds_via_argv:
        argv += ["--host", "127.0.0.1", "--port", str(port)]
    lines = _boot_lines(argv, env)

    startup = [line for line in lines if startup_pattern.match(line)]
    assert len(startup) == 1, (
        "the formatted startup line must appear exactly once; saw "
        f"{[line for line in lines if 'langfuse tracing enabled' in line]}"
    )
    runtime_pattern = re.compile(
        r"\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3} INFO nyx\.agent: "
        rf"prometheus exporter listening on :{metrics_port}/metrics$"
    )
    runtime = [line for line in lines if runtime_pattern.match(line)]
    assert len(runtime) == 1, (
        "the runtime nyx.agent INFO line must render with the process format; "
        f"saw {[line for line in lines if 'prometheus exporter listening' in line]}"
    )
    for needle in (
        "Started server process",
        "Application startup complete.",
        "Uvicorn running on",
    ):
        assert sum(needle in line for line in lines) == 1, (
            f"uvicorn's {needle!r} line must not be duplicated; saw {lines}"
        )
