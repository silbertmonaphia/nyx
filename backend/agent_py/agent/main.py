"""FastAPI app factory for the Python agent service.

Routes:
  - POST /agent/run  — accepts ChatRequest, streams the SSE envelope byte-for-byte to the
    Go chat handler's response. The Go side then proxies these bytes to the browser
    unchanged. User id read from `X-Nyx-User-Id` (set by Go after JWT verification).
    The `Authorization` header is forwarded as a passthrough to the MCP and RAG
    bridges (which re-verify the JWT); the Python service itself does NOT verify it.
    The trust boundary at this hop is the private network — Python is reachable only
    by Go in compose / k8s deployments (see FUTURE_AGENT.md + docker-compose.yml).
  - GET  /healthz     — 200 with `{"status":"ok"}` when Redis is reachable; 503 otherwise.
  - GET  /metrics     — Prometheus counters.
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import uuid
from contextlib import asynccontextmanager
from typing import AsyncIterator

from fastapi import FastAPI, HTTPException, Request, Response
from fastapi.responses import StreamingResponse
from prometheus_client import generate_latest, CONTENT_TYPE_LATEST
from redis.asyncio import Redis

from .budget import BudgetGuard
from .config import Settings, get_settings
from .graph import build_graph, run_agent_stream
from .observability import setup_observability, shutdown_observability
from .rag_client import RagClient
from .ratelimit import IpRateLimiter, RateLimitMiddleware
from .retention import RetentionCron
from .run_metadata import RunMetadata, RunMetadataUnavailable, build_pool
from .sweeper import StaleRunSweeper
from .state import new_state
from .tools_client import ToolsClient
from .types import (
    ChatRequest,
    Event,
    NoteEvent,
    RunIDEvent,
)

log = logging.getLogger("nyx.agent")

_UUID4_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")


@asynccontextmanager
async def lifespan(app: FastAPI):
    settings = get_settings()

    tracer = setup_observability(settings)

    # Spin up a dedicated Prometheus exporter on `metrics_port` (default 9001).
    # `prometheus_client.start_http_server` runs its own daemon thread; we don't
    # await it. The compose Prometheus target scrapes `agent:9001/metrics`.
    from prometheus_client import start_http_server as _prom_start

    try:
        _prom_start(settings.metrics_port)
    except OSError as exc:  # noqa: BLE001 — port already bound across tests
        if "Address already in use" not in str(exc):
            raise
    log.info("prometheus exporter listening on :%d/metrics", settings.metrics_port)

    redis = Redis.from_url(settings.redis_url, decode_responses=True)
    app.state.redis = redis
    app.state.budget = BudgetGuard(redis=redis, cap=settings.budget_per_user_per_hour)
    app.state.tracer = tracer
    app.state.settings = settings

    # Run-state persistence: only initialise when `database_url` is set
    # (allows local dev without Postgres). Migrations run automatically on
    # startup — same posture as the Go backend's golang-migrate on boot —
    # unless `PYTHON_AGENT_MIGRATIONS_DISABLED=true` (compose prod path
    # relies on the dedicated `agent-migrate` one-shot service instead).
    run_metadata: RunMetadata | None = None
    checkpointer = None
    retention: RetentionCron | None = None
    sweeper: StaleRunSweeper | None = None
    if settings.database_url:
        if not settings.migrations_disabled:
            from migrations._dev_upgrade_hook import run_upgrade_head

            await run_upgrade_head()

        pool = await build_pool(
            settings.database_url, max_size=settings.database_pool_max_size
        )
        # Fail fast on a bad DSN — try one query before the lifespan returns
        # so an operator with a misconfigured URL sees the error at startup
        # rather than on the first /agent/run. asyncpg pools are lazy so this
        # is the cheapest "is this DSN actually reachable" check we can run.
        try:
            async with pool.acquire(
                timeout=settings.database_pool_acquire_timeout_seconds
            ) as conn:
                await conn.execute("SELECT 1")
        except Exception as exc:  # noqa: BLE001
            log.error("database connectivity check failed: %r", exc)
            raise
        run_metadata = RunMetadata(
            pool, acquire_timeout=settings.database_pool_acquire_timeout_seconds
        )

        # langGraph PostgresSaver — psycopg-based, so we strip the
        # URL `options=...` (set search_path via SQL instead) and SET
        # the search_path on the saver's connection so its setup() lands
        # checkpoint_* tables in `langgraph.*`.
        checkpointer = await _build_checkpointer(settings.database_url)
        await _set_search_path_async(checkpointer.conn, "langgraph")
        await checkpointer.setup()

        if settings.retention_enabled and run_metadata is not None:
            retention = RetentionCron(run_metadata, checkpointer, settings=settings)
            retention.start()

        # Stale-run sweeper: reaps zombie `running` rows. Takes no
        # checkpointer on purpose — reaping never deletes checkpoint data
        # (retention collects it with the row); see sweeper.py.
        if settings.sweep_enabled and run_metadata is not None:
            sweeper = StaleRunSweeper(run_metadata, settings)
            sweeper.start()
    else:
        # No DB configured — agent runs are NOT persisted. Run state
        # will not survive a process restart, and `run_id` echo won't
        # enable resume. Loud warning so operators don't think
        # persistence is on when it isn't. To enable: set
        # `PYTHON_AGENT_DATABASE_URL` in `backend/.env`.
        log.warning(
            "PYTHON_AGENT_DATABASE_URL is unset; agent runs are NOT persisted. "
            "Process restart loses all in-flight state and `run_id` echo "
            "will not enable resume. Set the env var to enable persistence."
        )

    app.state.run_metadata = run_metadata
    app.state.checkpointer = checkpointer
    app.state.retention = retention
    app.state.sweeper = sweeper

    log.info(
        "agent_py started",
        extra={
            "port": settings.port,
            "metrics_port": settings.metrics_port,
            "llm_model": settings.llm_model,
            "planner_model": settings.planner_model,
            "budget_cap": settings.budget_per_user_per_hour,
            "run_state_enabled": run_metadata is not None,
        },
    )
    try:
        yield
    finally:
        if sweeper is not None:
            await sweeper.stop()
        if retention is not None:
            await retention.stop()
        if checkpointer is not None:
            try:
                await checkpointer.__aexit__(None, None, None)
            except Exception:  # noqa: BLE001
                pass
        if run_metadata is not None:
            # `pool` is local to the `if settings.database_url:` branch above;
            # reaching here means it was created. Closing it releases all
            # connections back to Postgres. Without this, the agent leaks
            # one pool per restart in dev.
            try:
                await pool.close()
            except Exception as exc:  # noqa: BLE001
                log.warning("failed to close asyncpg pool cleanly: %r", exc)
        shutdown_observability()
        await redis.aclose()


async def _build_checkpointer(database_url: str):
    """Build and return an open AsyncPostgresSaver usable as a graph checkpointer.

    langGraph's `AsyncPostgresSaver.from_conn_string` is psycopg-based; psycopg's
    URL parser rejects `=` inside `options=...` (we use `options=-c search_path=...`
    for asyncpg). Strip the options before handing to langGraph; main.py will
    `SET search_path` on the saver connection right after `setup()`.
    """
    import re as _re

    from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver
    from langgraph.checkpoint.serde.jsonplus import JsonPlusSerializer

    # Our AgentState channels hold custom agent.types pydantic objects
    # (`Plan`/`PlanStep`/`StepKind`, `Event`, `ChatMessage`). langGraph's
    # msgpack serde logs "Deserializing unregistered type ... will be
    # blocked in a future version" for module-local types — register them
    # now via the serializer's allowlist. Without this, a future
    # langgraph bump would refuse to decode our checkpoints and resume
    # would break. (Contract verified against the installed langgraph:
    # `JsonPlusSerializer(allowed_msgpack_modules=[...])` and
    # `from_conn_string(..., serde=...)` are both keyword-only.)
    serde = JsonPlusSerializer(
        allowed_msgpack_modules=[
            ("agent.types", "Plan"),
            ("agent.types", "PlanStep"),
            ("agent.types", "StepKind"),
            ("agent.types", "ChatMessage"),
            ("agent.types", "Event"),
        ]
    )

    # Strip both encoded and unencoded `options=...` from the URL.
    saver_url = _re.sub(r"&?options=[^&]*", "", database_url)
    saver_url = _re.sub(r"\?$", "", saver_url)

    saver_cm = AsyncPostgresSaver.from_conn_string(saver_url, serde=serde)
    saver = await saver_cm.__aenter__()
    # Stash the context manager so the lifespan teardown can close it.
    saver._cm = saver_cm  # type: ignore[attr-defined]
    return saver


async def _set_search_path_async(conn, schema: str) -> None:
    """Set search_path on a psycopg AsyncConnection.

    langGraph's `AsyncPostgresSaver.conn` is a psycopg AsyncConnection;
    `.execute()` is async and must be awaited. We `SET search_path` so
    langGraph's `setup()` (which creates unqualified `checkpoint_*`
    tables) lands them in our schema rather than `public`.

    Deliberately NO `public` in the path (matches the DSN validator and
    `migrations/env.py`): the agent must be structurally unable to touch
    Go's `public.*` tables. The saver only needs its own schema —
    builtins resolve via `pg_catalog`, which is always searched first.
    """
    await conn.execute(f"SET search_path TO {schema}")


async def _heartbeat_loop(
    run_metadata: RunMetadata, run_id: str, interval_seconds: int
) -> None:
    """Refresh the run's heartbeat every `interval_seconds` until cancelled.

    First touch is immediate (covers resume — a resumed run re-freshens
    its row before any step can stall). Failures are logged and the loop
    continues: the heartbeat is best-effort liveness metadata and must
    never fail the run (`openspec/changes/agent-stale-run-sweeper`). A
    False return means the row went terminal — stop quietly.
    """
    log.info(
        "heartbeat loop started: run_id=%s interval=%ss", run_id, interval_seconds
    )
    while True:
        try:
            touched = await run_metadata.touch_heartbeat(run_id)
            if not touched:
                return
        except asyncio.CancelledError:
            raise
        except RunMetadataUnavailable as exc:
            log.warning(
                "heartbeat touch failed (non-fatal): run_id=%s err=%r",
                run_id,
                exc,
            )
        except Exception as exc:  # noqa: BLE001 — never let heartbeat kill the run
            log.warning(
                "heartbeat touch raised unexpectedly: run_id=%s err=%r",
                run_id,
                exc,
            )
        await asyncio.sleep(interval_seconds)


def create_app(settings: Settings | None = None) -> FastAPI:
    settings = settings or get_settings()
    app = FastAPI(title="nyx-agent", lifespan=lifespan)

    # Per-IP rate limit on /agent/run only. /healthz and /metrics stay
    # open for compose / Prometheus scraping. See ratelimit.py for the
    # rationale (defense-in-depth for the no-auth-at-this-hop trust
    # boundary).
    app.add_middleware(
        RateLimitMiddleware,
        limiter=IpRateLimiter(
            requests_per_second=settings.requests_per_second,
            burst=settings.burst,
        ),
    )

    @app.get("/healthz")
    async def healthz() -> Response:
        redis: Redis | None = getattr(app.state, "redis", None)
        if redis is None:
            return Response(status_code=503, content='{"status":"starting"}')
        # LOW-11: ping Redis AND the LLM provider. Per spec
        # `agent-python-service` requirement "fail when the LLM provider is
        # unreachable AND the budget Redis client is unreachable". Cheap
        # GET to `{base}/v1/models` — OpenAI proper and vLLM both honour it.
        try:
            await redis.ping()
        except Exception as exc:  # noqa: BLE001
            log.warning("healthz: redis ping failed: %r", exc)
            return Response(status_code=503, content='{"status":"redis_unavailable"}')
        try:
            import httpx
            async with httpx.AsyncClient(timeout=2.0) as client:
                resp = await client.get(f"{settings.llm_base_url.rstrip('/')}/models")
                if resp.status_code >= 500:
                    return Response(
                        status_code=503,
                        content=f'{{"status":"llm_unavailable","http":{resp.status_code}}}',
                    )
        except Exception as exc:  # noqa: BLE001
            log.warning("healthz: llm ping failed: %r", exc)
            return Response(status_code=503, content='{"status":"llm_unavailable"}')
        return Response(status_code=200, content='{"status":"ok"}')

    @app.get("/metrics")
    async def metrics() -> Response:
        return Response(content=generate_latest(), media_type=CONTENT_TYPE_LATEST)

    @app.post("/agent/run")
    async def agent_run(req: Request) -> StreamingResponse:
        # Trust model: this hop is on the private network (compose bridge / k8s
        # NetworkPolicy restricting ingress to the backend). Python does NOT
        # verify the JWT — Go's chat handler has already verified it. We read
        # the user id from `X-Nyx-User-Id` (set by Go after verification) and
        # forward the Authorization bearer token as a passthrough to the MCP
        # and RAG bridges, which re-verify it themselves.
        #
        # The 400 below is NOT an auth check (this hop has no auth). It exists
        # to make a misconfigured caller fail loudly with a clear message
        # rather than silently running with user_id=0 — the budget guard keys
        # on user_id, and 0 would either bypass enforcement or collide with
        # any real user whose id happens to be 0.
        user_id_raw = req.headers.get("x-nyx-user-id", "").strip()
        if not user_id_raw:
            raise HTTPException(
                status_code=400,
                detail="missing X-Nyx-User-Id header (set by the Go chat handler)",
            )
        try:
            user_id = int(user_id_raw)
        except ValueError as exc:
            raise HTTPException(
                status_code=400,
                detail=f"X-Nyx-User-Id must be an integer, got {user_id_raw!r}",
            ) from exc

        # Passthrough for downstream JWT-scoped bridges (Go re-verifies on /mcp
        # and /internal/rag/retrieve). We never decode the token.
        bearer_token = req.headers.get("authorization", "").removeprefix("Bearer ").strip()

        try:
            body = await req.json()
        except json.JSONDecodeError as exc:
            raise HTTPException(status_code=400, detail="invalid JSON body") from exc

        try:
            chat_req = ChatRequest.model_validate(body)
        except Exception as exc:  # noqa: BLE001
            raise HTTPException(status_code=400, detail=f"invalid request: {exc}") from exc

        return StreamingResponse(
            _stream_agent_run(
                chat_req,
                user_id=user_id,
                bearer_token=bearer_token,
                request_id=req.headers.get("x-request-id"),
                app=app,
                settings=settings,
            ),
            media_type="text/event-stream",
            headers={
                "Cache-Control": "no-cache",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
            },
        )

    return app


async def _stream_agent_run(
    chat_req: ChatRequest,
    *,
    user_id: int,
    bearer_token: str,
    request_id: str | None,
    app: FastAPI,
    settings: Settings,
) -> AsyncIterator[bytes]:
    """Run the agent and yield SSE frames incrementally as the graph progresses.

    Frames are emitted in the wire shape the frontend parser (chatService.ts:174-403)
    consumes:
        `event: <kind>\ndata: <flat payload>\n\n`

    The flat payload is the typed event object (NOT a `{kind, data}` wrapper).
    We yield frames as soon as the LangGraph graph emits a new state snapshot
    (via `run_agent_stream`) so a slow step doesn't block earlier frames from
    reaching the client. After the final frames, we emit the sentinel
    `data: [DONE]` which the parser uses to yield its internal terminator.

    Run-state resume (see openspec/changes/agent-run-state/):
      - First frame of every run is `event: run_id` carrying the server-minted uuid4.
      - If the client echoed a non-terminal `run_id`, we emit `note{kind:"resumed", ...}`
        before `plan` and seed the graph state from langGraph's checkpoint.
      - On terminal `done` we mark `agent.runs.status='completed'`; on `error{code != agent_unavailable}`
        we mark `'failed'`. langGraph's checkpoint tables carry the state itself.
    """
    run_metadata: RunMetadata | None = getattr(app.state, "run_metadata", None)
    checkpointer = getattr(app.state, "checkpointer", None)

    # Mint or resume run_id.
    run_id = chat_req.run_id
    resumed = False
    if run_metadata is not None and run_id is not None:
        existing = await run_metadata.get_run(run_id, user_id)
        if existing is not None and existing.status not in ("completed", "failed"):
            resumed = True
        else:
            # Wrong user, not found, or terminal — fall through to fresh path.
            run_id = None

    if run_id is None:
        if run_metadata is not None:
            try:
                run_id = await run_metadata.create_run(user_id)
            except RunMetadataUnavailable as exc:
                log.warning("create_run failed; running without persistence: %r", exc)
                run_id = str(uuid.uuid4())
        else:
            run_id = str(uuid.uuid4())

    # First frame: server-minted run_id (echoed so client can resume later).
    yield _sse_encode(
        Event(
            kind="run_id",
            payload=RunIDEvent(run_id=run_id).model_dump(),
        )
    )

    # Resume note (before plan frame).
    if resumed:
        yield _sse_encode(
            Event(
                kind="note",
                payload=NoteEvent(
                    text="resumed",
                    kind="resumed",
                    last_step_id=None,  # graph rehydrates the right node itself
                    run_id=run_id,
                ).model_dump(),
            )
        )
    else:
        yield _sse_encode(
            Event(
                kind="note",
                payload=NoteEvent(
                    text="fresh_run",
                    kind="fresh_run",
                    run_id=run_id,
                ).model_dump(),
            )
        )

    state = new_state(
        user_id=user_id,
        run_id=run_id,
        request_id=request_id,
        messages=chat_req.messages,
        model=chat_req.model,
    )

    # CRITICAL-3: thread the user's bearer token into the RAG + tool clients.
    rag = RagClient(base_url=settings.rag_bridge_url, bearer_token=bearer_token)
    tools = ToolsClient(base_url=settings.mcp_url, bearer_token=bearer_token)

    # Build the graph with the checkpointer if persistence is enabled.
    # Re-build per-request because the saver needs `thread_id` per-run.
    graph = build_graph(
        settings=settings,
        budget=app.state.budget,
        rag=rag,
        tools=tools,
        checkpointer=checkpointer,
    )

    graph_config: dict | None = None
    if checkpointer is not None:
        graph_config = {"configurable": {"thread_id": run_id}}

    # Liveness heartbeat: while this stream owns the run, refresh
    # `last_heartbeat_at` so the stale-run sweeper can tell a live run
    # from a crash (openspec/changes/agent-stale-run-sweeper). Started
    # immediately before the try so the task can never leak: the only
    # awaits before this point are the two client-visible opening frames,
    # and a disconnect there exits the generator before the task exists.
    heartbeat_task: asyncio.Task[None] | None = None
    if run_metadata is not None:
        heartbeat_task = asyncio.create_task(
            _heartbeat_loop(
                run_metadata, run_id, settings.heartbeat_interval_seconds
            ),
            name=f"agent-heartbeat-{run_id}",
        )

    total_tokens = 0
    error_code: str | None = None
    saw_done = False
    try:
        async for event in run_agent_stream(
            state,
            settings=settings,
            tracer=app.state.tracer,
            budget=app.state.budget,
            rag=rag,
            tools=tools,
            graph=graph,
            graph_config=graph_config,
            resumed=resumed,
        ):
            yield _sse_encode(event)
            # Track terminal totals for metadata persistence.
            if event.kind == "done":
                usage = event.payload.get("usage") or {}
                total_tokens = int(usage.get("total_tokens", 0))
                saw_done = True
            elif event.kind == "error":
                error_code = event.payload.get("code")
    finally:
        # Stop the heartbeat before the terminal write so a late touch
        # cannot interleave with (or outlive) the terminal transition.
        if heartbeat_task is not None:
            heartbeat_task.cancel()
            try:
                await heartbeat_task
            except asyncio.CancelledError:
                pass

        # Persist terminal metadata if enabled. Non-fatal: emit a typed
        # error event only if the user-facing run is still in flight and
        # the metadata failure is the cause.
        if run_metadata is not None:
            try:
                if error_code is not None and error_code != "agent_unavailable":
                    await run_metadata.mark_failed(run_id, error_code)
                elif error_code is None and saw_done:
                    await run_metadata.mark_completed(run_id, total_tokens)
                # error_code == "agent_unavailable", or a stream closed before
                # `done` (client disconnect / server cancellation) → leave the
                # row `running`: the run is still resumable inside the window,
                # and the stale-run sweeper reaps it if nobody comes back
                # (test-review C1, 2026-10-14: this branch used to mark
                # mid-run disconnects `completed`, which the sweeper and the
                # retention GC then trusted).
            except RunMetadataUnavailable as exc:
                log.warning(
                    "metadata terminal write failed (non-fatal): run_id=%s err=%r",
                    run_id,
                    exc,
                )
                yield _sse_encode(
                    Event(
                        kind="error",
                        payload={
                            "error": "run metadata persistence failed",
                            "code": "run_metadata_unavailable",
                            "request_id": request_id,
                        },
                    )
                )

    # Frontend parser triggers its own terminator on `data: [DONE]`. Old clients
    # that listen on `event: terminator` still work because we emit that event too.
    yield b"data: [DONE]\n\n"


def _sse_encode(event: Event) -> bytes:
    """Render an Event into the SSE wire shape: `event: <kind>\ndata: <flat payload>\n\n`.

    The payload is `event.payload` directly — the same flat dict the frontend
    parser destructures. Field names per `frontend/src/features/chat/services/
    chatService.ts:174-403`:
      - delta         → {delta: "<text>"}
      - plan          → {steps: [...]}
      - step_start    → {step_id, kind}
      - tool_call     → {step_id, call_id, name, args}
      - tool_result   → {step_id, call_id, ok, result, error}
      - step_end      → {step_id, status, reason}
      - note          → {text, provider?, kind?, last_step_id?, run_id?, failed_steps?}
      - done          → {usage: {prompt_tokens, completion_tokens, total_tokens}}
      - error         → {error, code, request_id}
      - terminator    → {reason}
    """
    data = json.dumps(event.payload, separators=(",", ":"))
    return f"event: {event.kind}\ndata: {data}\n\n".encode("utf-8")


app = create_app()


if __name__ == "__main__":
    import uvicorn

    settings = get_settings()
    uvicorn.run(
        "agent.main:app",
        host="0.0.0.0",
        port=settings.port,
        log_level="info",
    )