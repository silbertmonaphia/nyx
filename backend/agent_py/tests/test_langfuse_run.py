"""Integration tests: a full agent run through the SSE endpoint with Langfuse
tracing enabled, asserting on the spans the client actually exports.

The LLM here is a REAL langchain `BaseChatModel` fake (not the bare duck-type
used elsewhere) so the LangChain callback machinery fires and the Langfuse
CallbackHandler attached in `main.py` captures real generations — the point
of these tests is the wiring, not the model.

openspec/changes/agent-langfuse-tracing: tasks 3.3 (generations + identity),
3.4 (step/tool spans), 3.5 (fail-open delivery).
"""

from __future__ import annotations

import json
import os
import uuid
from contextlib import asynccontextmanager
from typing import Any, ClassVar

import httpx
import pytest
from fakeredis import aioredis as fakeaioredis
from langchain_core.language_models.chat_models import BaseChatModel
from langchain_core.messages import AIMessage
from langchain_core.outputs import ChatGeneration, ChatResult
from opentelemetry.sdk.trace.export import SpanExporter
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from agent import observability as obs_module
from agent.config import Settings
from agent.main import create_app
from agent.types import ChatMessage, ChatRequest

_PLAN: dict[str, Any] = {
    "steps": [
        {"id": "step_1", "kind": "tool", "tool": "list_feeds", "args": {}},
        {"id": "step_2", "kind": "llm", "prompt": "summarise"},
    ]
}

_USAGE = {"input_tokens": 11, "output_tokens": 22, "total_tokens": 33}


class _FakeChatModel(BaseChatModel):
    """Real langchain BaseChatModel: emits a canned plan on call 1 (planner)
    and a summary afterwards (`llm` step). Carries provider-reported usage so
    the generation-capture test can assert it end to end."""

    model_config: ClassVar[dict[str, Any]] = {"arbitrary_types_allowed": True, "extra": "allow"}

    call_count: ClassVar[int] = 0

    @property
    def _llm_type(self) -> str:
        return "fake-langfuse-test"

    def _generate(self, messages, stop=None, run_manager=None, **kwargs) -> ChatResult:
        type(self).call_count += 1
        if type(self).call_count == 1:
            content = json.dumps(_PLAN)
        else:
            content = "summary: no feeds"
        message = AIMessage(content=content, usage_metadata=dict(_USAGE))
        return ChatResult(generations=[ChatGeneration(message=message)])

    def bind_tools(self, tools, **kwargs):
        # BaseChatModel.bind_tools raises NotImplementedError in
        # langchain-core 1.x (ChatOpenAI implements it); the executor's
        # react + re-prompt paths call it, so fakes must too. Bound kwargs
        # are ignored by `_generate`.
        return self.bind(tools=tools, **kwargs)


class _ReactFakeChatModel(BaseChatModel):
    """Planner call 1 emits a react-step plan; the react iterations then emit
    a tool call, then a final answer — so each react model call surfaces as
    its own generation (design D3 names the react loop as the propagation
    risk this pins)."""

    model_config: ClassVar[dict[str, Any]] = {"arbitrary_types_allowed": True, "extra": "allow"}

    call_count: ClassVar[int] = 0
    _PLAN: ClassVar[dict[str, Any]] = {
        "steps": [
            {
                "id": "react_1",
                "kind": "react",
                "tool": "list_feeds",
                "args": {},
                "max_iterations": 2,
            }
        ]
    }

    @property
    def _llm_type(self) -> str:
        return "fake-react-langfuse-test"

    def _generate(self, messages, stop=None, run_manager=None, **kwargs) -> ChatResult:
        type(self).call_count += 1
        n = type(self).call_count
        if n == 1:
            message = AIMessage(content=json.dumps(self._PLAN), usage_metadata=dict(_USAGE))
        elif n == 2:
            message = AIMessage(
                content="",
                tool_calls=[{"id": "call_2", "name": "list_feeds", "args": {}}],
                usage_metadata=dict(_USAGE),
            )
        else:
            message = AIMessage(content="react summary", usage_metadata=dict(_USAGE))
        return ChatResult(generations=[ChatGeneration(message=message)])

    def bind_tools(self, tools, **kwargs):
        return self.bind(tools=tools, **kwargs)


class _RaisingExporter(SpanExporter):
    """Every export attempt fails — simulates an unreachable Langfuse."""

    def export(self, spans):
        raise RuntimeError("langfuse unreachable (test)")

    def shutdown(self):
        pass


def _make_settings(mode: str, database_url: str = "") -> Settings:
    suffix = uuid.uuid4().hex[:8]
    return Settings(
        port=9000,
        metrics_port=9004,  # off the default so parallel tests can't collide
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
        database_url=database_url,
        langfuse_enabled=(mode != "disabled"),
        # Unique per test: the SDK keeps a process-wide registry keyed by
        # public key, so reuse across tests would leak shut-down managers.
        langfuse_public_key=f"pk-lf-run-{suffix}",
        langfuse_secret_key=f"sk-lf-run-{suffix}",
        langfuse_host="http://127.0.0.1:9",
    )


@asynccontextmanager
async def _app_ctx(
    monkeypatch,
    mode: str = "enabled",
    fail_tools: bool | str = False,
    chat_model: type[BaseChatModel] | None = None,
    database_url: str = "",
):
    """Build the app wired for tracing per `mode`:
    enabled | disabled | raising (every span export fails).

    `fail_tools=True` makes every tool dispatch fail as transient;
    `fail_tools="arg"` makes it fail as a registry rejection (drives the
    model re-prompt path). `chat_model` overrides the LLM fake."""
    chat_model = chat_model or _FakeChatModel
    exporter: SpanExporter
    exporter = _RaisingExporter() if mode == "raising" else InMemorySpanExporter()

    redis = fakeaioredis.FakeRedis(decode_responses=True)
    settings = _make_settings(mode, database_url=database_url)

    from agent import config as config_module

    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    monkeypatch.setattr(config_module, "_settings", settings)

    import agent.executor as executor_module
    import agent.planner as planner_module
    import agent.rag_client as rag_module
    import agent.tools_client as tools_module

    class _StubRag:
        async def retrieve(self, query):
            from agent.rag_client import RagResult

            return RagResult(context_block="stub context", citations=[])

    class _StubTools:
        async def call(self, name, arguments):
            if fail_tools == "arg":
                from agent.tools_client import ToolError

                raise ToolError("arguments rejected by registry (test)")
            if fail_tools:
                from agent.tools_client import ToolUnavailableError

                raise ToolUnavailableError("tool backend down (test)")
            return {"items": [1, 2, 3]}

        async def list_tools(self):
            return [
                {
                    "type": "function",
                    # readOnlyHint keeps the confirmation gate out of the
                    # picture: these tests exercise retry/tracing behavior
                    # for a read tool (agent-feed-tools D3).
                    "annotations": {"readOnlyHint": True},
                    "function": {
                        "name": "list_feeds",
                        "description": "List user's feeds",
                        "parameters": {"type": "object", "properties": {}},
                    },
                }
            ]

    rag_module.RagClient = lambda *a, **kw: _StubRag()
    tools_module.ToolsClient = lambda *a, **kw: _StubTools()

    import agent.main as main_module

    # main.py binds both clients by from-import (`from .tools_client import
    # ToolsClient`), so patching the source modules alone never reaches the
    # per-request construction at main.py:493-494 — without these two lines
    # the runs below make REAL HTTP calls to the stub URLs and the tool
    # steps fail as transient 502s (observed: "MCP 502" through the dev
    # proxy). Same from-import trap as setup_observability/setup_langfuse.
    monkeypatch.setattr(main_module, "RagClient", lambda *a, **kw: _StubRag())
    monkeypatch.setattr(main_module, "ToolsClient", lambda *a, **kw: _StubTools())

    _FakeChatModel.call_count = 0
    chat_model.call_count = 0
    planner_module.ChatOpenAI = chat_model
    executor_module.ChatOpenAI = chat_model

    # Capture the Jaeger-side spans too: the regression these tests guard is
    # that enabling tracing must not re-parent the Jaeger tree (the langfuse
    # callback handler writes its spans into the same provider-agnostic OTel
    # context slot). The tracer is handed to the app explicitly, so a global
    # registration conflict across tests (OTel allows one set_tracer_provider)
    # does not matter here.
    jaeger_exporter = InMemorySpanExporter()

    def _capturing_setup(settings):
        from opentelemetry import trace as otel_trace
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor

        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(jaeger_exporter))
        otel_trace.set_tracer_provider(provider)
        return provider.get_tracer("nyx-agent-test")

    def _setup_langfuse_with_exporter(settings):
        return obs_module.setup_langfuse(settings, span_exporter=exporter)

    # Patch BOTH bindings: `main` did a from-import, so replacing the
    # attribute on the observability module alone does not reach the
    # lifespan's call sites (the from-import trap documented in the
    # agent-stale-run-sweeper suite notes).
    monkeypatch.setattr(obs_module, "setup_observability", _capturing_setup)
    monkeypatch.setattr(main_module, "setup_observability", _capturing_setup)
    monkeypatch.setattr(main_module, "setup_langfuse", _setup_langfuse_with_exporter)

    app = create_app(settings)
    # Same convention as tests/test_run.py's `app_and_state.lm` — test state
    # rides on the yielded app object.
    app.exporter = exporter
    app.jaeger_exporter = jaeger_exporter
    try:
        async with app.router.lifespan_context(app):
            app.state.redis = redis
            from agent.budget import BudgetGuard

            app.state.budget = BudgetGuard(redis=redis, cap=1000000)
            yield app
    finally:
        obs_module.shutdown_langfuse()
        # The SDK keeps a process-wide registry keyed by public key and does
        # NOT de-register on shutdown. Left alone, a later test's
        # `get_client()` would find >=2 instances and silently hand the
        # callback handler a tracing-disabled client. Tests own the process,
        # so clear it.
        try:
            from langfuse._client.resource_manager import LangfuseResourceManager

            LangfuseResourceManager._instances.clear()
        except Exception:  # pragma: no cover - SDK internals moved  # noqa: BLE001, S110
            pass
        await redis.aclose()


@pytest.fixture
async def traced_app(monkeypatch):
    async with _app_ctx(monkeypatch, mode="enabled") as app:
        yield app


async def _post_chat(app, body, user_id: int = 42):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        return await client.post(
            "/agent/run",
            json=body,
            headers={
                "X-Nyx-User-Id": str(user_id),
                "Authorization": "Bearer stub-token-not-verified",
            },
        )


def _chat_body() -> dict:
    return ChatRequest(messages=[ChatMessage(role="user", content="hi")]).model_dump()


def _run_id_from_stream(text: str) -> str:
    for line in text.splitlines():
        if line.startswith("data: {"):
            payload = json.loads(line.removeprefix("data: "))
            if "run_id" in payload:
                return payload["run_id"]
    raise AssertionError(f"no run_id frame in stream: {text[:400]!r}")


def _frame_kinds(text: str) -> list[str]:
    return [
        line.removeprefix("event: ")
        for line in text.splitlines()
        if line.startswith("event: ")
    ]


def _attr(span, key: str):
    return span.attributes.get(key)


def _generations(exporter: InMemorySpanExporter) -> list:
    return [
        s
        for s in exporter.get_finished_spans()
        if _attr(s, "langfuse.observation.type") == "generation"
    ]


def _jaeger_tree(exporter: InMemorySpanExporter) -> list[tuple[str, str | None]]:
    """(span name, parent span name) pairs — the Jaeger-side structure.
    A non-null parent absent from the export renders as 'DANGLING'."""
    spans = exporter.get_finished_spans()
    by_id = {s.context.span_id: s for s in spans}
    out = []
    for s in spans:
        if s.parent is None:
            out.append((s.name, None))
        else:
            parent = by_id.get(s.parent.span_id)
            out.append((s.name, parent.name if parent else "DANGLING"))
    return sorted(out)


def _normalized_agent_tree(exporter: InMemorySpanExporter) -> list[tuple[str, str]]:
    """The agent.* subtree with parents outside it normalized to 'external'.

    External parents differ between two apps in one process for a harness
    reason (the ASGI server span resolves against the global provider —
    whichever app in the process registered one first), so the invariant
    under test is the agent-internal structure, not the name of the
    server-span parent."""
    tree = _jaeger_tree(exporter)
    agent_names = {name for name, _ in tree if name.startswith("agent.")}
    return sorted(
        (name, parent if parent in agent_names else "external")
        for name, parent in tree
        if name.startswith("agent.")
    )


@pytest.mark.asyncio
async def test_run_exports_session_user_generations_and_steps(traced_app):
    """Spec: generations with prompts/replies/usage (R "Every model call is
    captured as a generation") + identity (R "Traces carry run and user
    identity") + step/tool structure (R "Plan steps and tool calls are
    traced")."""
    app = traced_app
    resp = await _post_chat(app, _chat_body())
    assert resp.status_code == 200
    assert "event: done\n" in resp.text
    run_id = _run_id_from_stream(resp.text)

    obs_module._langfuse_client.flush()
    spans = traced_app.exporter.get_finished_spans()
    assert spans, "expected the run to export spans"

    # Identity: every captured span carries the run as session and the
    # Go-verified user; the root (LangGraph) span too.
    session_spans = [s for s in spans if _attr(s, "session.id") == run_id]
    assert session_spans, "no span carried session.id == run_id"
    assert all(
        _attr(s, "user.id") == "42" for s in session_spans
    ), "session spans must all carry the verified user id"

    # Generations: planner + executor model calls, each with input, output,
    # model name, and provider-reported usage.
    generations = [
        s for s in spans if _attr(s, "langfuse.observation.type") == "generation"
    ]
    assert len(generations) >= 2, f"expected planner + executor generations, got {len(generations)}"
    for gen in generations:
        assert _attr(gen, "langfuse.observation.model.name") == "stub-model"
        assert _attr(gen, "langfuse.observation.input"), "generation lost its input"
        assert _attr(gen, "langfuse.observation.output"), "generation lost its output"
        usage = _attr(gen, "langfuse.observation.usage_details")
        if isinstance(usage, str):
            usage = json.loads(usage)
        assert usage.get("total") == _USAGE["total_tokens"], (
            f"provider usage not captured: {usage!r}"
        )

    # Steps + tools: our own observation wrapper (task 3.4).
    names = [s.name for s in spans]
    assert "agent.step.tool" in names, f"tool step span missing; got {names}"
    tool_spans = [s for s in spans if s.name == "agent.step.tool"]
    assert any(_attr(s, "langfuse.observation.metadata.tool") == "list_feeds" for s in tool_spans), (
        "tool span must name the dispatched tool"
    )
    assert "agent.step.llm" in names, f"llm step span missing; got {names}"

    # Consumer-side credential sweep (spec "Credentials never reach the
    # trace backend"): no exported attribute anywhere in the run's tree may
    # contain a credential-shaped string. The caller's Authorization bearer
    # was live on this request — if any path leaked it into traces, this is
    # where it surfaces.
    from agent.observability import _CREDENTIAL_PATTERNS

    for span in spans:
        for key, value in span.attributes.items():
            text = str(value)
            for pattern in _CREDENTIAL_PATTERNS:
                assert not pattern.search(text), (
                    f"credential-shaped value leaked into exported attribute "
                    f"{span.name}.{key}"
                )


@pytest.mark.asyncio
async def test_disabled_tracing_exports_nothing(monkeypatch):
    """Spec "Tracing is opt-in and off by default": disabled → no client, no
    spans, run completes normally."""
    async with _app_ctx(monkeypatch, mode="disabled") as app:
        resp = await _post_chat(app, _chat_body())
        assert resp.status_code == 200
        assert "event: done\n" in resp.text
        assert obs_module._langfuse_client is None
        assert app.exporter.get_finished_spans() == ()


@pytest.mark.asyncio
async def test_failed_tool_dispatch_marks_spans(monkeypatch):
    """Spec "Plan steps and tool calls are traced" — failed-tool scenario: a
    dispatch that exhausts its transient retries yields failed tool spans
    (per attempt) and a failed step span, all under the run's session."""
    async with _app_ctx(monkeypatch, mode="enabled", fail_tools=True) as app:
        resp = await _post_chat(app, _chat_body())
        # Flush inside the context: teardown shuts the client down.
        obs_module._langfuse_client.flush()
        spans = app.exporter.get_finished_spans()
    assert resp.status_code == 200
    run_id = _run_id_from_stream(resp.text)

    tool_calls = [s for s in spans if s.name == "agent.tool_call"]
    assert len(tool_calls) == 3, (  # initial + tool_retry_max(2) retries
        f"expected one span per dispatch attempt; got {[s.name for s in spans]}"
    )
    # Session/user attributes ride the trace root; the dispatch spans must
    # belong to the SAME trace (tree), not duplicate the attributes.
    session_spans = [s for s in spans if _attr(s, "session.id") == run_id]
    assert session_spans, "no span carried session.id == run_id"
    run_trace_id = session_spans[0].context.trace_id
    for span in tool_calls:
        assert span.context.trace_id == run_trace_id, "tool span escaped the run's trace"
        assert _attr(span, "langfuse.observation.metadata.tool") == "list_feeds"
        out = _attr(span, "langfuse.observation.output")
        assert "failed" in str(out), f"attempt span not marked failed: {out!r}"
        assert _attr(span, "langfuse.observation.level") == "ERROR", (
            "failed attempt span must carry ERROR level"
        )

    step_spans = [s for s in spans if s.name == "agent.step.tool"]
    assert step_spans, "step span missing"
    step_out = _attr(step_spans[0], "langfuse.observation.output")
    assert "failed" in str(step_out), f"step span not marked failed: {step_out!r}"
    assert _attr(step_spans[0], "langfuse.observation.level") == "ERROR", (
        "failed step span must carry ERROR level"
    )


@pytest.mark.asyncio
async def test_export_failures_never_change_the_run(monkeypatch):
    """Spec "Trace delivery is best-effort and never affects runs": with every
    export failing, the SSE frame sequence is identical to a tracing-disabled
    run and the run still reaches done + terminator."""
    async with _app_ctx(monkeypatch, mode="raising") as raising_app:
        raising_resp = await _post_chat(raising_app, _chat_body())
    assert raising_resp.status_code == 200

    async with _app_ctx(monkeypatch, mode="disabled") as disabled_app:
        disabled_resp = await _post_chat(disabled_app, _chat_body())
    assert disabled_resp.status_code == 200

    assert _frame_kinds(raising_resp.text) == _frame_kinds(disabled_resp.text)
    assert "event: done\n" in raising_resp.text
    assert "event: terminator\n" in raising_resp.text
    # Frame KINDS only: run ids differ per request, and the spec's
    # "byte-identical" claim is scoped to the tracing-enabled vs
    # tracing-disabled shape of the same run — the kind sequence is the
    # part that must not move (payload shapes are pinned by test_run.py).


@pytest.mark.asyncio
async def test_react_iterations_are_each_captured(monkeypatch):
    """Spec "Every model call is captured as a generation" — the react-loop
    scenario: each model invocation inside the loop is its own generation
    (design D3 named this path as the propagation risk)."""
    async with _app_ctx(monkeypatch, mode="enabled", chat_model=_ReactFakeChatModel) as app:
        resp = await _post_chat(app, _chat_body())
        obs_module._langfuse_client.flush()
        spans = app.exporter.get_finished_spans()
    assert resp.status_code == 200

    generations = [
        s for s in spans if _attr(s, "langfuse.observation.type") == "generation"
    ]
    # planner + react iteration 1 (tool call) + react iteration 2 (answer).
    assert len(generations) >= 3, (
        f"expected planner + 2 react generations; got "
        f"{[s.name for s in spans]}"
    )
    names = [s.name for s in spans]
    assert "agent.step.react" in names, f"react span missing; got {names}"


@pytest.mark.asyncio
async def test_tool_reprompt_model_call_is_captured(monkeypatch):
    """Spec "Every model call is captured as a generation" — the tool
    re-prompt path: a registry-rejected tool call triggers a model re-prompt
    (`_reprompt_for_corrected_call`), whose LLM invocation must also surface
    as a generation. The fake returns text (no corrected call), so the step
    terminates with the terminal failure — but the re-prompt call happened
    and must be visible."""
    async with _app_ctx(monkeypatch, mode="enabled", fail_tools="arg") as app:
        resp = await _post_chat(app, _chat_body())
        obs_module._langfuse_client.flush()
        spans = app.exporter.get_finished_spans()
    assert resp.status_code == 200

    generations = [
        s for s in spans if _attr(s, "langfuse.observation.type") == "generation"
    ]
    # planner + re-prompt + the answering llm step.
    assert len(generations) >= 3, (
        f"expected planner + re-prompt + executor generations; got "
        f"{[s.name for s in spans]}"
    )


@pytest.mark.asyncio
async def test_enabling_tracing_does_not_touch_the_jaeger_tree(monkeypatch):
    """Regression (review C1): the langfuse callback handler writes spans
    into the shared, provider-agnostic OTel context slot — enabling tracing
    must NOT re-parent (or re-trace-id) the Jaeger tree our own spans build.
    Asserts the enabled run's span tree is structurally identical to the
    disabled run's, with no dangling parents."""
    async with _app_ctx(monkeypatch, mode="enabled") as app:
        resp = await _post_chat(app, _chat_body())
        jaeger_enabled = _jaeger_tree(app.jaeger_exporter)
    assert resp.status_code == 200

    async with _app_ctx(monkeypatch, mode="disabled") as app_disabled:
        resp2 = await _post_chat(app_disabled, _chat_body())
        jaeger_disabled = _jaeger_tree(app_disabled.jaeger_exporter)
    assert resp2.status_code == 200

    assert jaeger_enabled, "no Jaeger-side spans captured"
    names = {name for name, _parent in jaeger_enabled}
    assert {"agent.step.plan", "agent.step.tool", "agent.step.llm", "agent.budget"} <= names
    # The C1 regression, detected directly and order-independently: no
    # Jaeger span may be parented to ANY langfuse span id (langfuse spans
    # never reach the Jaeger exporter, so such children render as orphaned
    # roots).
    langfuse_ids = {s.context.span_id for s in app.exporter.get_finished_spans()}
    re_parented = [
        s.name
        for s in app.jaeger_exporter.get_finished_spans()
        if s.parent is not None and s.parent.span_id in langfuse_ids
    ]
    assert not re_parented, (
        f"Jaeger spans re-parented onto langfuse-only span ids: {re_parented}"
    )
    # And the agent-side structure matches the tracing-disabled run exactly.
    assert _normalized_agent_tree(app.jaeger_exporter) == _normalized_agent_tree(
        app_disabled.jaeger_exporter
    ), (
        "enabling tracing changed the Jaeger span tree:\n"
        f"enabled:  {jaeger_enabled}\ndisabled: {jaeger_disabled}"
    )


_PG_DSN = os.environ.get("TEST_ASYNC_PG_DSN")


@pytest.mark.skipif(
    not _PG_DSN,
    reason="TEST_ASYNC_PG_DSN not set; resume needs a live Postgres",
)
@pytest.mark.asyncio
async def test_resumed_run_keeps_langfuse_session(monkeypatch):
    """Spec "Traces carry run and user identity" — the resumed-run scenario:
    an echoed run_id resumes the crashed run, and the resumed portion's
    trace carries the SAME session id (the run_id), not a fresh one."""
    dsn = _PG_DSN
    assert dsn is not None

    async with _app_ctx(monkeypatch, mode="enabled", database_url=dsn) as app:
        body = _chat_body()
        run_id = None
        # Phase 1: drive the SSE generator directly (the httpx ASGITransport
        # runs the route to completion — no real disconnect) and close it
        # right after the plan frame. That GeneratorExit-at-an-in-try-yield
        # is the disconnect-without-done path, which leaves the row
        # `running` — and therefore resumable (same trick as
        # tests/test_sweeper.py's disconnect test).
        import agent.main as main_module

        gen = main_module._stream_agent_run(
            ChatRequest(messages=[ChatMessage(role="user", content="hi")]),
            user_id=42,
            bearer_token="stub-token-not-verified",
            request_id=None,
            app=app,
            settings=app.state.settings,
        )
        try:
            for _ in range(50):
                frame = await gen.__anext__()
                text = frame.decode() if isinstance(frame, (bytes, bytearray)) else frame
                for line in text.splitlines():
                    if line.startswith("data: {") and "run_id" in line:
                        run_id = json.loads(line.removeprefix("data: "))["run_id"]
                if "event: plan" in text:
                    break
            else:
                pytest.fail("plan frame never arrived")
        finally:
            await gen.aclose()
        assert run_id, "phase 1 never saw a run_id frame"

        # Phase 2: echo the run_id — the service resumes the same run.
        # Reset the fake's call counter so the resumed run's (re-)planning
        # gets the plan payload again.
        _FakeChatModel.call_count = 0
        resumed = await _post_chat(app, {**body, "run_id": run_id})
        assert resumed.status_code == 200
        assert '"resumed"' in resumed.text
        obs_module._langfuse_client.flush()
        spans = app.exporter.get_finished_spans()

    # Both phases' spans live under the one session id (the run_id).
    session_spans = [s for s in spans if _attr(s, "session.id") == run_id]
    assert session_spans, (
        "resumed run lost its session id; sessions seen: "
        f"{ {_attr(s, 'session.id') for s in spans if _attr(s, 'session.id')} }"
    )
    assert any(_attr(s, "user.id") == "42" for s in session_spans)
