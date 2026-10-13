"""End-to-end /agent/run test — drives the FastAPI app with httpx.AsyncClient + MockTransport.

Replaces the legacy `httpx.MockTransport` based tests; runs the planner + executor end-to-end
with a stubbed LLM response. Verifies SSE frame sequence on the wire.
"""

from __future__ import annotations

import json
from typing import Any, ClassVar

import httpx
import langchain_openai
import pytest
from fakeredis import aioredis as fakeaioredis

from agent.config import Settings
from agent.main import create_app
from agent.types import ChatMessage, ChatRequest


class _StubStructuredChain:
    """Shim for the planner's bound chain
    (`base.bind(response_format=...) | JsonOutputParser() | RunnableLambda(...)`).

    `__or__` chaining is ignored; `ainvoke` returns the canned plan
    validated into `Plan` — what the real chain's parser + validator
    produce.
    """

    def __init__(self, plan_payload: dict):
        self._payload = plan_payload

    def __or__(self, other):
        return self

    async def ainvoke(self, messages):
        from agent.types import Plan

        return Plan.model_validate(self._payload)


class _StubLlm:
    """Drop-in replacement for langchain_openai.ChatOpenAI used in the planner.

    The planner reaches the LLM via `bind(response_format=...)` (structured
    path); the executor still uses plain `ainvoke`. Both consume the same
    canned plan so the call numbering matches the pre-structured flow.
    """
    _PLAN: ClassVar[dict[str, Any]] = {
        "steps": [
            {"id": "step_1", "kind": "tool", "tool": "list_feeds", "args": {}},
            {"id": "step_2", "kind": "llm", "prompt": "summarise"},
        ]
    }
    # Class-level: the planner and the executor each construct their own
    # instance, so the counter must span both — call 1 is the plan (planner
    # `bind`), call 2+ is the answering text (executor `ainvoke`).
    call_count = 0

    def __init__(self, *args, **kwargs):
        pass

    async def ainvoke(self, messages):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        if type(self).call_count == 1:
            return AIMessage(content=json.dumps(self._PLAN))
        return AIMessage(content="summary: no feeds")

    def bind(self, **kwargs):
        # Planner path (`_build_structured_client` binds `response_format`
        # and chains parsers): consume the call-1 slot, as the legacy flow
        # did.
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)


class _LoopForeverLlm:
    """React-cap test stub. The planner call returns a 1-step react plan; subsequent
    calls always emit a tool_call — so the react loop runs until the iteration cap hits.

    Implemented as a *class* so the patched `ChatOpenAI` binding is callable
    (matching the planner's `client = ChatOpenAI(...)` instantiation).
    """
    call_count = 0  # class-level; each instance shares count for the run
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

    def __init__(self, *args, **kwargs):
        pass

    async def ainvoke(self, messages):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        n = type(self).call_count
        if n == 1:
            return AIMessage(content=json.dumps(self._PLAN))
        # Subsequent calls: emit a tool call (LangChain bind_tools shape).
        return AIMessage(
            content="",
            tool_calls=[
                {
                    "id": f"call_{n}",
                    "name": "list_feeds",
                    "args": {},
                }
            ],
        )

    def bind(self, **kwargs):
        # Planner path: consume the class-level call-1 slot so the
        # executor's react loop still starts at n == 2.
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)


class _EmptyAnswerPlanLlm:
    """Planner stub: rag-only plan; executor stub: empty answering text.

    The parser appends an answering `llm` step to data-only plans
    ([rag] becomes [rag_1, final_answer(llm)]), so the empty-answer path
    is reached via that answering step producing no text — the second
    `ainvoke` returns "". Pins the `empty_answer` guard in
    `run_agent_stream`.
    """

    call_count = 0
    _PLAN: ClassVar[dict[str, Any]] = {"steps": [{"id": "rag_1", "kind": "rag", "query": "feeds"}]}

    def __init__(self, *args, **kwargs):
        pass

    async def ainvoke(self, messages):
        from langchain_core.messages import AIMessage

        type(self).call_count += 1
        if type(self).call_count == 1:
            return AIMessage(content=json.dumps(self._PLAN))
        return AIMessage(content="")

    def bind(self, **kwargs):
        # Planner path: consume the class-level call-1 slot so the
        # executor's answering step still receives "" (call 2).
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)


@pytest.fixture
def settings():
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
    )


@pytest.fixture
async def app_and_state(settings, monkeypatch):
    """Build the app with a stubbed ChatOpenAI + stubbed Redis + stubbed HTTP clients.

    `app_and_state.lm` is the LLM stub the test sets before issuing its request.
    """
    redis = fakeaioredis.FakeRedis(decode_responses=True)

    # get_settings() caches a module-level singleton; the lifespan uses
    # that singleton rather than the fixture's Settings instance. Patch
    # get_settings() so the lifespan sees the fixture's configuration.
    from agent import config as config_module

    monkeypatch.setattr(config_module, "get_settings", lambda: settings)
    monkeypatch.setattr(config_module, "_settings", settings)

    # Patch ChatOpenAI inside agent.planner (where the planner imported it) so the
    # planner's `client = ChatOpenAI(...)` call uses our stub. Patching the
    # `langchain_openai.ChatOpenAI` module attribute doesn't reach the planner's
    # already-bound local reference.
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
            return {"items": [1, 2, 3]}

        async def list_tools(self):
            # Return OpenAI-format tool schemas so bind_tools() succeeds.
            return [
                {
                    "type": "function",
                    "function": {
                        "name": "list_feeds",
                        "description": "List user's feeds",
                        "parameters": {"type": "object", "properties": {}},
                    },
                }
            ]

    rag_module.RagClient = lambda *a, **kw: _StubRag()
    tools_module.ToolsClient = lambda *a, **kw: _StubTools()

    # Test sets the LLM stub via the `lm` attribute on the fixture.
    _StubLlm.call_count = 0  # class-level counter: reset between tests
    app_and_state.lm = _StubLlm()
    planner_module.ChatOpenAI = _StubLlm
    executor_module.ChatOpenAI = _StubLlm

    # Also short-circuit OTel export — the test env has no Jaeger at :4318 and the
    # retry/back-off cycle slows the suite down by 10+ seconds.
    from agent import observability as obs_module
    orig_setup = obs_module.setup_observability

    def _noop_setup(settings):
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider

        provider = TracerProvider()
        trace.set_tracer_provider(provider)
        return trace.get_tracer("nyx-agent-test")

    obs_module.setup_observability = _noop_setup

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
        rag_module.RagClient = rag_module.RagClient.__class__  # restore class binding
        tools_module.ToolsClient = tools_module.ToolsClient.__class__
        obs_module.setup_observability = orig_setup
        await redis.aclose()


def _set_lm(app_and_state, lm_cls):
    """Bind a fresh LLM *class* for the next test request."""
    import agent.executor as executor_module
    import agent.planner as planner_module

    planner_module.ChatOpenAI = lm_cls
    executor_module.ChatOpenAI = lm_cls
    if hasattr(lm_cls, "call_count"):
        lm_cls.call_count = 0  # reset between tests
    app_and_state.lm = lm_cls()


async def _post_chat(app, body, user_id: int = 42):
    """POST /agent/run with X-Nyx-User-Id set (the way Go forwards it after JWT verify).

    Authorization is still included as a passthrough so the test exercises the
    RagClient/ToolsClient code paths that thread the bearer token to /mcp and
    /internal/rag/retrieve. The token value is irrelevant — the Python service
    does not verify it.
    """
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


def _parsed_frames(body: str):
    """Yield every JSON-decodable `data:` frame payload from a body."""
    out = []
    for ln in body.splitlines():
        if not ln.startswith("data: ") or ln == "data: [DONE]":
            continue
        try:
            out.append(json.loads(ln.removeprefix("data: ")))
        except json.JSONDecodeError:
            pass
    return out


@pytest.mark.asyncio
async def test_agent_run_emits_sse_frame_sequence(app_and_state):
    app = app_and_state

    body = ChatRequest(messages=[ChatMessage(role="user", content="hi")]).model_dump()
    resp = await _post_chat(app, body)
    assert resp.status_code == 200
    text = resp.text

    # Slice 2 (agent-run-state): the stream opens with the server-minted
    # `run_id` frame and a `note{kind:"fresh_run"}` before the plan.
    # Old token-stream clients ignore unknown kinds — the envelope shape
    # (`event: <kind>\ndata: <flat json>\n\n`) is unchanged.
    assert text.startswith("event: run_id\n")

    frames = _parsed_frames(text)
    assert frames, f"expected at least one data frame; raw: {text!r}"
    # Frame 0: run_id payload. Frame 1: fresh-run note. Frame 2: plan.
    assert "run_id" in frames[0] and isinstance(frames[0]["run_id"], str)
    assert frames[1].get("kind") == "fresh_run", (
        f"second frame must announce a fresh run; parsed: {frames[:2]}"
    )
    assert "steps" in frames[2], f"third frame must be the plan; parsed: {frames[:3]}"
    assert isinstance(frames[2]["steps"], list)
    assert frames[2]["steps"][0]["id"] == "step_1"

    # `done`, `terminator`, and the sentinel `[DONE]` must all be present.
    assert "event: done\n" in text
    assert "event: terminator\n" in text
    assert "data: [DONE]\n\n" in text

    # delta frame carries `{"delta": "<text>"}` flat, not `{"kind":..., "data":...}`.
    deltas = [f for f in frames if isinstance(f, dict) and "delta" in f]
    assert deltas, f"expected a delta frame on the wire; parsed: {frames}"
    assert isinstance(deltas[0]["delta"], str)
    # done frame carries `{"usage": {...}}` flat.
    dones = [f for f in frames if isinstance(f, dict) and "usage" in f]
    assert dones, "expected a done frame with usage on the wire"
    assert "total_tokens" in dones[0]["usage"]


@pytest.mark.asyncio
async def test_react_step_emits_iteration_cap_error(app_and_state):
    """T-1: spec §"Nested ReAct caps iterations" — when the model loops forever, the
    react step must terminate with `error{code:"react_iterations_exceeded"}` plus a
    `step_end{status:"failed",reason:"react_max_iterations"}` frame.
    """
    app = app_and_state
    # Replace with a LLM that always emits a tool_call on subsequent calls.
    _set_lm(app_and_state, _LoopForeverLlm)

    resp = await _post_chat(
        app,
        ChatRequest(messages=[ChatMessage(role="user", content="loop")]).model_dump(),
    )
    assert resp.status_code == 200
    frames = _parsed_frames(resp.text)
    # step_start frame for the react step.
    step_starts = [f for f in frames if isinstance(f, dict) and f.get("step_id") and f.get("kind") == "react"]
    tool_calls = [f for f in frames if "call_id" in f and f.get("name") == "list_feeds"]
    errors = [f for f in frames if isinstance(f, dict) and f.get("code") == "react_iterations_exceeded"]
    step_ends = [
        f for f in frames
        if isinstance(f, dict)
        and f.get("step_id") == "react_1"
        and f.get("status") == "failed"
    ]
    assert step_starts, f"expected at least one react step_start; got: {frames}"
    assert tool_calls, "react loop should dispatch at least one tool call before capping"
    assert errors, f"expected react_iterations_exceeded error; got: {frames}"
    assert step_ends, f"expected step_end(failed, react_max_iterations); got: {frames}"

    # step_start frame for the react step.
    step_starts = [f for f in frames if isinstance(f, dict) and f.get("step_id") and f.get("kind") == "react"]
    assert step_starts, f"expected at least one react step_start; got: {frames}"

    # At least one tool_call / tool_result pair (the loop dispatches each iteration).
    assert tool_calls, "react loop should dispatch at least one tool call before capping"

    # Cap hit: error frame carries `code:"react_iterations_exceeded"`.
    assert errors, f"expected react_iterations_exceeded error; got: {frames}"

    # step_end frame for the react step carries `status:"failed"` and the reason
    # (the executor wraps in a RuntimeError, so the reason contains "react_max_iterations").
    step_ends = [
        f for f in frames
        if isinstance(f, dict)
        and f.get("step_id") == "react_1"
        and f.get("status") == "failed"
        and "react_max_iterations" in str(f.get("reason", ""))
    ]
    assert step_ends, f"expected step_end(failed, ...); got: {frames}"


@pytest.mark.asyncio
async def test_rag_only_plan_emits_empty_answer_error(app_and_state):
    """A run that produces no visible text must not finish green.

    2026-10 report: "summarize my feeds" planned a single rag step; the
    retrieved context went to step_outputs (write-only at the time) and
    the stream ended with done + zero deltas — the user saw a step and
    no answer, and the run row read `completed`. The parser now appends
    an answering llm step to data-only plans, so this test reaches the
    guard via that step producing empty text. The guard in
    `run_agent_stream` emits error{code:"empty_answer"} so the client
    shows a failure rather than an empty bubble.
    """
    app = app_and_state
    _set_lm(app_and_state, _EmptyAnswerPlanLlm)

    resp = await _post_chat(
        app,
        ChatRequest(
            messages=[ChatMessage(role="user", content="summarize my feeds")]
        ).model_dump(),
    )
    assert resp.status_code == 200
    frames = _parsed_frames(resp.text)
    errors = [f for f in frames if isinstance(f, dict) and f.get("code") == "empty_answer"]
    assert errors, f"expected empty_answer error; got: {frames}"
    # Terminal frames still follow so old clients close the stream cleanly.
    assert "event: done\n" in resp.text
    assert "data: [DONE]\n\n" in resp.text


class _TooLargePlanLlm:
    """Planner stub: an 11-step plan exceeds the default PLAN_MAX_STEPS (10),
    so the planner terminates with error{code:"plan_too_large"} before any
    step runs. The executor never reaches the model."""

    _PLAN: ClassVar[dict[str, Any]] = {
        "steps": [{"id": f"step_{i}", "kind": "llm", "prompt": "x"} for i in range(11)]
    }
    call_count = 0

    def __init__(self, *args, **kwargs):
        pass

    async def ainvoke(self, messages):
        raise AssertionError("executor must not run for a plan_too_large run")

    def bind(self, **kwargs):
        type(self).call_count += 1
        return _StubStructuredChain(self._PLAN)


@pytest.mark.asyncio
async def test_plan_too_large_stream_carries_partial_failure_note(app_and_state):
    """F1 regression (test-review round 1): the plan-level partial-failure
    note must appear IN THE STREAM, between the error frame and done.

    `run_agent_stream` slices `events_so_far` positionally per node chunk —
    a note inserted into the planner's already-yielded chunk (e.g. from the
    executor node, where this was originally emitted) never reaches the
    wire. The original unit test inspected state, not the stream, and passed
    while the wire carried no note (self-consistent wrongness)."""
    app = app_and_state
    _set_lm(app_and_state, _TooLargePlanLlm)

    resp = await _post_chat(
        app,
        ChatRequest(messages=[ChatMessage(role="user", content="hi")]).model_dump(),
    )
    assert resp.status_code == 200
    frames = _parsed_frames(resp.text)

    error_idx = next(
        i
        for i, f in enumerate(frames)
        if isinstance(f, dict) and f.get("code") == "plan_too_large"
    )
    note_idx = next(
        i
        for i, f in enumerate(frames)
        if isinstance(f, dict) and f.get("kind") == "partial_failure"
    )
    done_idx = next(i for i, f in enumerate(frames) if isinstance(f, dict) and "usage" in f)
    assert error_idx < note_idx < done_idx, f"frame order: error@{error_idx} note@{note_idx} done@{done_idx}"
    assert frames[note_idx]["failed_steps"] == []
    # Exactly one terminator frame — the pre-fix bug re-yielded it via the
    # positional slice (review round 2 root-cause finding); pin it.
    terminators = [f for f in frames if isinstance(f, dict) and list(f.keys()) == ["reason"]]
    assert len(terminators) == 1, f"expected exactly one terminator; got {terminators}"


@pytest.mark.asyncio
async def test_agent_run_missing_user_id_header_returns_400(app_and_state):
    """The Python service does not verify JWTs — Go sets X-Nyx-User-Id after
    JWT verification. A caller that omits the header gets a 400 (NOT 401 —
    this hop has no auth, just a sanity check so user_id never silently
    defaults to 0 and collides with the budget guard's per-user keying).
    """
    app = app_and_state
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post(
            "/agent/run",
            json=ChatRequest(messages=[ChatMessage(role="user", content="hi")]).model_dump(),
            headers={"Authorization": "Bearer whatever"},
        )
    assert resp.status_code == 400
    assert "X-Nyx-User-Id" in resp.text


@pytest.mark.asyncio
async def test_agent_run_non_integer_user_id_header_returns_400(app_and_state):
    """Defense against a misconfigured caller sending garbage in X-Nyx-User-Id."""
    app = app_and_state
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        resp = await client.post(
            "/agent/run",
            json=ChatRequest(messages=[ChatMessage(role="user", content="hi")]).model_dump(),
            headers={
                "X-Nyx-User-Id": "not-a-number",
                "Authorization": "Bearer whatever",
            },
        )
    assert resp.status_code == 400
    assert "must be an integer" in resp.text


@pytest.mark.asyncio
async def test_lifespan_shutdown_survives_pool_close_failure(monkeypatch):
    """Regression (review ticket): the pool-close error handler logged an
    unbound `exc`, so a raising `pool.close()` produced a NameError that
    aborted the remaining lifespan-shutdown steps (observability flush +
    redis close) — exactly when cleanup had already failed once."""
    from agent import main as main_module

    class _FakeConn:
        async def execute(self, *_args, **_kwargs):
            return None

    class _RaisingPool:
        class _Ctx:
            async def __aenter__(self):
                return _FakeConn()

            async def __aexit__(self, *_exc):
                return None

        def acquire(self, timeout=None):
            return self._Ctx()

        async def close(self):
            raise RuntimeError("pool explosion")

    class _FakeCheckpointer:
        def __init__(self):
            self.conn = object()

        async def setup(self):
            return None

        async def __aexit__(self, *_exc):
            return None

    async def _fake_build_pool(*_args, **_kwargs):
        return _RaisingPool()

    async def _fake_build_checkpointer(_url):
        return _FakeCheckpointer()

    async def _fake_set_search_path(_conn, _schema):
        return None

    monkeypatch.setattr(main_module, "build_pool", _fake_build_pool)
    monkeypatch.setattr(main_module, "_build_checkpointer", _fake_build_checkpointer)
    monkeypatch.setattr(main_module, "_set_search_path_async", _fake_set_search_path)

    shutdown_ran = {"observability": False}
    monkeypatch.setattr(
        main_module,
        "shutdown_observability",
        lambda: shutdown_ran.__setitem__("observability", True),
    )

    settings = Settings(
        _env_file=None,
        llm_base_url="http://stub",
        metrics_port=0,
        database_url="postgresql://u:p@localhost:5432/db",
        migrations_disabled=True,
        retention_enabled=False,
    )
    monkeypatch.setattr(main_module, "get_settings", lambda: settings)

    app = main_module.create_app(settings)
    async with main_module.lifespan(app):
        pass

    # Reaching here proves the NameError is gone; the flag proves shutdown
    # continued past the raised pool-close instead of aborting.
    assert shutdown_ran["observability"] is True