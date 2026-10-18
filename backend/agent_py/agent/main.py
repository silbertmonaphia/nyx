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
  - GET/POST/DELETE /agent/sessions* — chat-session REST surface (list, replay,
    create, resume, soft-delete), registered from `agent.api.chat_sessions`.
    Same trust model as /agent/run: owner-scoped via `X-Nyx-User-Id`; the
    ChatStore wired by the lifespan (`app.state.chat_store`) serves the reads.
  - GET  /agent/me/export, DELETE /agent/me — GDPR data-subject surface
    (export + right to erasure), registered from `agent.api.gdpr`. Same
    trust model; gated by `PYTHON_AGENT_GDPR_ENABLED` (503 when off). The
    audit trail lives in `agent.data_audit_log` (`app.state.audit_log`).
  - POST /agent/approvals — mutating-call approval decisions
    (openspec/changes/agent-feed-tools), registered from
    `agent.approvals_api`. Same trust model; records the decision in the
    shared Redis broker the paused run waits on (fail-closed everywhere).
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException, Request, Response
from fastapi.responses import StreamingResponse
from langchain_core.messages import HumanMessage, SystemMessage
from langchain_openai import ChatOpenAI
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from pydantic import SecretStr
from redis.asyncio import Redis

from .api.chat_sessions import router as chat_sessions_router
from .api.gdpr import router as gdpr_router
from .approvals import ApprovalGate, ApprovalStore, RunSink
from .approvals_api import router as approvals_router
from .audit import AuditLog
from .budget import BudgetGuard
from .chat_store import ChatStore, ChatStoreUnavailable
from .config import Settings, get_settings
from .graph import build_graph, run_agent_stream
from .observability import (
    get_langfuse_handler,
    setup_langfuse,
    setup_observability,
    shutdown_langfuse,
    shutdown_observability,
)
from .prefs import (
    PrefsStore,
    PrefsStoreUnavailable,
    extract_preferences,
    looks_like_preference,
    prefs_block,
)
from .rag_client import RagClient
from .ratelimit import IpRateLimiter, RateLimitMiddleware
from .retention import RetentionCron
from .run_metadata import RunMetadata, RunMetadataUnavailable, build_pool
from .state import AgentState, new_state
from .step_trace import StepTrace
from .sweeper import StaleRunSweeper
from .tool_dispatch import LocalToolRegistry, ToolDispatcher
from .tools_client import ToolsClient
from .types import (
    ChatRequest,
    Event,
    NoteEvent,
    RunIDEvent,
)
from .web_tools import build_local_tools

log = logging.getLogger("nyx.agent")

_LOG_FORMAT = "%(asctime)s %(levelname)s %(name)s: %(message)s"


def configure_logging() -> None:
    """Give the process a formatted root handler before uvicorn starts.

    uvicorn's LOGGING_CONFIG only configures its own `uvicorn.*` loggers, so
    without a root handler every record above `logging.lastResort`'s
    threshold renders bare — no timestamp, level, or logger name — and the
    agent's own INFO lines (the Langfuse "tracing enabled (host=…)"
    announcement, the Prometheus listen line, "agent_py started") are
    dropped outright. Root at WARNING keeps third-party INFO chatter (httpx
    request lines) out while rendering the OTel exporter's retry/drop
    diagnostics in full; the agent's own loggers at INFO carry its
    operational lines — both the `agent` package (`agent.observability`)
    and the runtime's `nyx.agent.*` tree, whose descendant loggers inherit
    the `nyx.agent` level. Idempotent: `basicConfig` is a no-op once root
    has handlers (uvicorn's dictConfig runs after this in the
    `python -m agent.main` path and only touches `uvicorn.*` loggers), and
    the level assignments are absolute.
    """
    logging.basicConfig(level=logging.WARNING, format=_LOG_FORMAT)
    logging.getLogger("agent").setLevel(logging.INFO)
    logging.getLogger("nyx.agent").setLevel(logging.INFO)


configure_logging()

_UUID4_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")

# Strong refs to detached terminal-persistence tasks (see the finally
# block in `_stream_agent_run`): asyncio tasks are weakly referenced, and
# a client-disconnect cancellation detaches the task while it is still
# writing — without this set it could be GC'd mid-write.
_FINALIZE_TASKS: set[asyncio.Task[None]] = set()

# Strong refs to detached preference-extraction tasks
# (openspec/changes/agent-user-preferences, design D2). Same hazard, same
# fix as `_FINALIZE_TASKS`: the task is spawned from `_stream_agent_run`'s
# finally (after the reply is finalized) and holds no reference to the
# response generator, so a client disconnect cannot cancel it — but only
# this set keeps it from being GC'd before it finishes writing prefs.
# Tests await this set draining (see tests/test_prefs_injection.py).
_PREFS_TASKS: set[asyncio.Task[None]] = set()

_TITLE_TASKS: set[asyncio.Task[None]] = set()

_TITLE_MAX_CHARS = 48
_TITLE_REFINEMENT_MAX_TOKENS = 64
_TITLE_REFINEMENT_TIMEOUT_SECONDS = 30

_TITLE_REFINEMENT_SYSTEM_PROMPT = """You title a chat session from its first exchange.

Return ONLY the title text — no quotes, no markdown, no preamble, no trailing punctuation.
At most 6 words (or 16 characters for Chinese/Japanese/Korean), naming the exchange's subject.
Use the language the user wrote in. Return the text only."""


def _derive_session_title(text: str) -> str:
    """Deterministic title label for a first user message.

    Whitespace (newlines included) collapses to single spaces, the result
    is trimmed, and it is truncated to `_TITLE_MAX_CHARS` CHARACTERS —
    Python slicing counts code points, so a multi-byte character is never
    split (the CJK-safe bound the spec pins).
    """
    collapsed = re.sub(r"\s+", " ", text or "").strip()
    return collapsed[:_TITLE_MAX_CHARS]


def _should_refine_title(
    *, saw_done: bool, title_written: bool, chat_store: ChatStore | None
) -> bool:
    """Whether this run spawns the one-shot title refinement (D2).

    `title_written` is True only for the run that recorded the session's
    deterministic first-exchange title — `set_title_if_empty` wins the
    `title=''` guard exactly once per session, so "at most once per
    session" is structural: a continuation, a resumed run, or a second
    turn never reaches the spawn. `saw_done` keeps failed / aborted runs
    (no completed exchange to summarise) out, and the store must be
    available for the write to mean anything.
    """
    return bool(saw_done and title_written and chat_store is not None)


async def _record_first_exchange_title(
    chat_store: ChatStore,
    *,
    run_id: str,
    user_id: int,
    content: str | None,
) -> bool:
    """Write the deterministic first-exchange title; False when it did not land.

    The `title=''` predicate in `set_title_if_empty` is what makes the
    call idempotent: every non-resumed run reaches it, but only the
    session's first exchange wins the write. A store failure is
    warn-logged and the run proceeds unaffected (openspec/changes/
    agent-chat-fixes "Chat store unavailable degrades silently").
    """
    title = _derive_session_title(content or "")
    if not title:
        return False
    try:
        return await chat_store.set_title_if_empty(run_id, user_id, title)
    except ChatStoreUnavailable as exc:
        log.warning(
            "chat_store.set_title_if_empty failed (non-fatal): "
            "run_id=%s err=%r",
            run_id,
            exc,
        )
        return False


async def _refine_session_title(
    chat_store: ChatStore,
    *,
    user_id: int,
    session_id: str,
    user_text: str,
    assistant_text: str,
    settings: Settings,
) -> None:
    """One small LLM call → refined session title. NEVER raises.

    Mirrors `extract_preferences` (openspec/changes/agent-chat-fixes D2):
    per-call `ChatOpenAI(settings.planner_model)` with bounded
    `max_tokens` / `timeout`, no retry, WARN on failure, and NO budget
    consultation — an internal operation, not user-visible chat work. Any
    failure (client construction, LLM error, empty or non-text output,
    store fault) leaves the deterministic title in place.
    """
    try:
        client = ChatOpenAI(
            model=settings.planner_model,
            base_url=settings.llm_base_url,
            api_key=SecretStr(settings.llm_api_key or "no-key"),
            temperature=0,
            max_tokens=_TITLE_REFINEMENT_MAX_TOKENS,
            timeout=_TITLE_REFINEMENT_TIMEOUT_SECONDS,
        )
        response = await client.ainvoke(
            [
                SystemMessage(content=_TITLE_REFINEMENT_SYSTEM_PROMPT),
                HumanMessage(
                    content=f"User: {user_text}\n\nAssistant: {assistant_text}"
                ),
            ]
        )
    except Exception as exc:  # noqa: BLE001 — fire-and-forget contract: never raise
        log.warning(
            "session title refinement failed (non-fatal): run_id=%s err=%r",
            session_id,
            exc,
        )
        return
    raw = response.content
    title = _derive_session_title(raw) if isinstance(raw, str) else ""
    if not title:
        log.warning(
            "session title refinement produced no usable title (non-fatal): "
            "run_id=%s content_type=%s",
            session_id,
            type(raw).__name__,
        )
        return
    try:
        await chat_store.set_title(session_id, user_id, title)
    except Exception as exc:  # noqa: BLE001 — warn-and-continue: the run is over
        log.warning(
            "session title refinement write failed (non-fatal): "
            "run_id=%s err=%r",
            session_id,
            exc,
        )


def _spawn_title_refinement(
    chat_store: ChatStore,
    *,
    user_id: int,
    run_id: str,
    user_text: str,
    assistant_text: str,
    settings: Settings,
) -> None:
    """Spawn the fire-and-forget title refinement, tracked in `_TITLE_TASKS`.

    Spawned from `_stream_agent_run`'s finally, strictly after
    `_persist_terminal` — the reply has already reached the client, and
    the task holds no reference to the response generator, so it can
    never delay, block, or alter the SSE stream. The module-level set is
    the strong ref that keeps asyncio (weak refs only) from GC'ing the
    task mid-write after a disconnect teardown, mirroring `_PREFS_TASKS`.
    """
    if not (user_text and assistant_text):
        return
    title_task = asyncio.create_task(
        _refine_session_title(
            chat_store,
            user_id=user_id,
            session_id=run_id,
            user_text=user_text,
            assistant_text=assistant_text,
            settings=settings,
        ),
        name=f"agent-title-{run_id}",
    )
    _TITLE_TASKS.add(title_task)
    title_task.add_done_callback(_TITLE_TASKS.discard)


@asynccontextmanager
async def lifespan(app: FastAPI):
    settings = get_settings()

    tracer = setup_observability(settings)

    # Opt-in LLM tracing (openspec/changes/agent-langfuse-tracing). Runs after
    # `setup_observability` on purpose: the global OTel provider is claimed by
    # the Jaeger pipeline first; langfuse builds its own isolated provider.
    setup_langfuse(settings)

    # Spin up a dedicated Prometheus exporter on `metrics_port` (default 9001).
    # `prometheus_client.start_http_server` runs its own daemon thread; we don't
    # await it. The compose Prometheus target scrapes `agent:9001/metrics`.
    from prometheus_client import start_http_server as _prom_start

    try:
        _prom_start(settings.metrics_port)
    except OSError as exc:
        if "Address already in use" not in str(exc):
            raise
    log.info("prometheus exporter listening on :%d/metrics", settings.metrics_port)

    redis = Redis.from_url(settings.redis_url, decode_responses=True)
    app.state.redis = redis
    app.state.budget = BudgetGuard(redis=redis, cap=settings.budget_per_user_per_hour)
    app.state.tracer = tracer
    app.state.settings = settings

    # Agent-native web tools (openspec/changes/agent-web-tools): the local
    # registry is built once from settings; per-request `ToolDispatcher`s
    # pair it with the request's bearer-scoped MCP client. Both flags
    # default off — an empty registry means the merged tool list equals
    # the MCP registry list and the planner prompt is unchanged.
    app.state.local_tools = LocalToolRegistry(build_local_tools(settings))
    if settings.web_fetch_allow_private:
        log.warning(
            "PYTHON_AGENT_WEB_FETCH_ALLOW_PRIVATE=true — the web_fetch SSRF "
            "guard is RELAXED (private addresses and non-80/443 ports are "
            "reachable). Dev/e2e only; never enable in a deployed environment."
        )

    # Run-state persistence: only initialise when `database_url` is set
    # (allows local dev without Postgres). Migrations run automatically on
    # startup — same posture as the Go backend's golang-migrate on boot —
    # unless `PYTHON_AGENT_MIGRATIONS_DISABLED=true` (compose prod path
    # relies on the dedicated `agent-migrate` one-shot service instead).
    run_metadata: RunMetadata | None = None
    chat_store: ChatStore | None = None
    prefs_store: PrefsStore | None = None
    audit_log: AuditLog | None = None
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
        except Exception as exc:
            log.error("database connectivity check failed: %r", exc)
            raise
        run_metadata = RunMetadata(
            pool, acquire_timeout=settings.database_pool_acquire_timeout_seconds
        )
        # Chat-history store (openspec/changes/agent-chat-history-storage).
        # Reuses the same pool — no second pool (design.md D9). Owns
        # `agent.chat_sessions` + `agent.chat_messages` for the SSE
        # generator writes and the retention cron.
        chat_store = ChatStore(
            pool, acquire_timeout=settings.database_pool_acquire_timeout_seconds
        )
        # Per-user preferences (openspec/changes/agent-user-preferences).
        # Reuses the same pool — no second pool. Serves the planner
        # injection read, the post-`done` extraction writes, and the
        # retention iteration's derived-memory cleanup.
        prefs_store = PrefsStore(
            pool, acquire_timeout=settings.database_pool_acquire_timeout_seconds
        )
        # GDPR audit trail (openspec/changes/agent-gdpr-compliance).
        # Reuses the same pool — no second pool. Owns
        # `agent.data_audit_log`: the append-only evidence for
        # export/erase (counts only, never content) plus its own
        # retention iteration on the cron below.
        audit_log = AuditLog(
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
            # ChatStore is required for the chat-retention iteration; when
            # chat retention is disabled, pass None so the iteration is
            # skipped (per design.md D5 + D7).
            retention_chat_store = (
                chat_store if settings.chat_retention_enabled else None
            )
            retention = RetentionCron(
                run_metadata,
                retention_chat_store,
                checkpointer,
                settings=settings,
                # Derived-memory cleanup rides the chat hard-delete
                # (agent-user-preferences D6). Passed regardless of
                # `prefs_enabled`: the kill switch stops extraction and
                # injection, but already-stored rows must still die with
                # their source session.
                prefs_store=prefs_store,
                # GDPR audit retention (agent-gdpr-compliance D5). Passed
                # regardless of `gdpr_enabled`: audit retention runs
                # independently of the endpoint kill switch, so the
                # evidence trail keeps moving while the endpoints are
                # frozen.
                audit_log=audit_log,
            )
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
    app.state.chat_store = chat_store
    app.state.prefs_store = prefs_store
    app.state.audit_log = audit_log
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
        shutdown_langfuse()
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
        except Exception as exc:
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

    # Chat-session REST surface (openspec/changes/agent-chat-history-ui):
    # GET/POST/DELETE under /agent/sessions. Handlers read the ChatStore
    # the lifespan put on `app.state.chat_store` (503 when unset — no DB
    # configured); the Go proxy owns the public /api/chat/sessions shape.
    app.include_router(chat_sessions_router)

    # GDPR data-subject surface (openspec/changes/agent-gdpr-compliance):
    # GET /agent/me/export + DELETE /agent/me. Same trust model as the
    # session surface; 503 when `PYTHON_AGENT_GDPR_ENABLED=false` (the
    # handlers short-circuit before touching any store). Private-network
    # only — no Go/SPA proxy in this slice.
    app.include_router(gdpr_router)

    # Mutating-call approval decisions (openspec/changes/agent-feed-tools):
    # the Go chat proxy posts approve/deny to POST /agent/approvals. Same
    # private-network trust model; the waiting run resolves through the
    # shared Redis broker (fail-closed when it is unavailable).
    app.include_router(approvals_router)

    return app


# Keep-alive cadence while a run produces no frame (agent-feed-tools
# D2/D6): a paused mutating call holds the stream open for up to the
# approval TTL, and intermediaries cut idle connections — an SSE comment
# is protocol-legal and skipped by the existing frontend parser.
_STREAM_KEEPALIVE_SECONDS = 15.0


async def _pump_events(state: AgentState, *, sink: RunSink, **kwargs) -> None:
    """Drive `run_agent_stream` in sink mode and close the channel at end.

    The graph's frames are delivered through the run's `RunSink` queue;
    this task exists so the SSE drain and the graph progress concurrently
    — which is what lets the gate's mid-node confirm prompt reach the
    client during the approval pause (and keeps frames flowing out of a
    slow node). Runs until exhaustion or cancellation; the `finally`
    always closes the channel so the drain can terminate.
    """
    try:
        async for _ in run_agent_stream(state, sink=sink, **kwargs):
            pass  # sink mode yields nothing — delivery rides the queue
    finally:
        await sink.close()


async def _drain_run_queue(
    queue: asyncio.Queue,
) -> AsyncIterator[tuple[str, object] | None]:
    """Yield the run's sink items in order, with an idle keep-alive.

    Items are `("event", Event)`, `("comment", str)`, or `None` (the run's
    channel closed). After `_STREAM_KEEPALIVE_SECONDS` without an item, a
    `("comment", "keep-alive")` is yielded so the connection stays warm
    while a run awaits an approval decision. The pending `queue.get()` is
    carried across keep-alive ticks (never cancelled mid-wait), so no item
    can be lost at a timeout boundary.
    """
    getter: asyncio.Task | None = None
    try:
        while True:
            getter = getter or asyncio.create_task(queue.get())
            done, _ = await asyncio.wait({getter}, timeout=_STREAM_KEEPALIVE_SECONDS)
            if not done:
                yield ("comment", "keep-alive")
                continue
            item = getter.result()
            getter = None
            if item is None:
                yield None
                return
            yield item
    finally:
        if getter is not None and not getter.done():
            getter.cancel()


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
    chat_store: ChatStore | None = getattr(app.state, "chat_store", None)
    prefs_store: PrefsStore | None = getattr(app.state, "prefs_store", None)
    checkpointer = getattr(app.state, "checkpointer", None)

    # Mint, resume, or continue the session (openspec/changes/agent-chat-history-ui).
    #
    # Three distinct meanings for an echoed id, in priority order:
    #   1. Live run (agent.runs non-terminal)  → crash-recovery resume
    #      (unchanged semantics; `resumed=True`).
    #   2. Live chat SESSION (chat_sessions row, non-deleted) → session
    #      continuation: the SPA echoed the session id, an explicit intent
    #      to append this turn to it (clicking a history row or typing in
    #      an open panel both count). The graph runs FRESH (new plan); the
    #      run-state layer is bypassed for this execution because
    #      agent.runs owns per-RUN rows and a terminal row must not be
    #      mutated (or a phantom "running" row minted).
    #   3. Neither → fresh session: mint a new run id (which doubles as
    #      the session id). The SPA's `+ New chat` is the only other
    #      session-boundary signal; there is no idle-time chopping — an
    #      echoed id is a deliberate action and must keep its thread.
    #
    # Sessions outlive runs since Slice 2: every turn of a conversation is
    # a new run but the same session, and a session created via
    # `POST /agent/sessions` has no `agent.runs` row at all until its
    # first turn — the chat_sessions check is what keeps the id.
    run_id = chat_req.run_id
    resumed = False
    session_continuation = False
    persist_run_state = True
    if run_metadata is not None and run_id is not None:
        existing = await run_metadata.get_run(run_id, user_id)
        if existing is not None and existing.status not in ("completed", "failed"):
            resumed = True
        else:
            # Not a live run — check whether the id is a live session.
            session = None
            if chat_store is not None:
                try:
                    session = await chat_store.get_session(run_id, user_id)
                except ChatStoreUnavailable as exc:
                    log.warning(
                        "chat_store.get_session failed during resume decision "
                        "(treating as fresh): run_id=%s err=%r",
                        run_id,
                        exc,
                    )
            if session is not None:
                session_continuation = True
                persist_run_state = False
            else:
                # Wrong user, unknown id, or soft-deleted session — fresh.
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

    # Chat-history writes (openspec/changes/agent-chat-history-storage).
    # Best-effort: failures are warn-logged and the stream continues
    # (design.md D7). The session row is the foundation for every
    # downstream message write — record_message cascades through
    # `ON DELETE CASCADE` to keep the slice self-contained.
    title_written = False
    if chat_store is not None:
        try:
            # `create_session` is idempotent (`ON CONFLICT (id) DO
            # NOTHING`), so it's safe to call on both fresh and resumed
            # runs. On resume, the row may have been written by a prior
            # process or it may be missing — both cases are handled.
            await chat_store.create_session(user_id, run_id)
            # Refresh `last_active_at` regardless of fresh/resumed.
            await chat_store.touch_session(run_id, user_id)
            # Record the inbound user message(s):
            #   - resumed: skip entirely — recorded at the original request
            #     entry; re-recording would duplicate.
            #   - session continuation: only the LAST user message. The SPA
            #     sends the full conversation history every turn, and the
            #     earlier ones are already stored in this session.
            #   - fresh session: every user message (the panel is empty, so
            #     in practice this is the one new message; a curl client
            #     importing a transcript still gets them all).
            if not resumed:
                user_msgs = [m for m in chat_req.messages if m.role == "user"]
                if session_continuation:
                    user_msgs = user_msgs[-1:]
                for msg in user_msgs:
                    try:
                        await chat_store.record_message(
                            run_id,
                            user_id,
                            role="user",
                            content=msg.content,
                        )
                    except ChatStoreUnavailable as exc:
                        log.warning(
                            "chat_store.record_message(user) failed "
                            "(non-fatal): run_id=%s err=%r",
                            run_id,
                            exc,
                        )
                title_written = await _record_first_exchange_title(
                    chat_store,
                    run_id=run_id,
                    user_id=user_id,
                    content=user_msgs[0].content if user_msgs else "",
                )
        except ChatStoreUnavailable as exc:
            log.warning(
                "chat_store session init failed (non-fatal): run_id=%s err=%r",
                run_id,
                exc,
            )

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
    # The dispatcher composes the agent-native tools (web tools, when
    # enabled) with the request's MCP client — local names win on collision
    # (openspec/changes/agent-web-tools, design D1/D3).
    rag = RagClient(base_url=settings.rag_bridge_url, bearer_token=bearer_token)
    tools = ToolDispatcher(
        mcp=ToolsClient(base_url=settings.mcp_url, bearer_token=bearer_token),
        local=getattr(app.state, "local_tools", None) or LocalToolRegistry(),
    )

    # Per-user preferences (openspec/changes/agent-user-preferences D5):
    # best-effort load of the accumulated set, rendered into the planner
    # system prompt. Failure — or the kill switch, or an empty set —
    # degrades to `""`, and the composed prompt is then byte-identical to
    # its pre-injection form (regression-pinned).
    prefs_text = ""
    if settings.prefs_enabled and prefs_store is not None:
        prefs: list = []
        try:
            prefs = await prefs_store.load_prefs(
                user_id, limit=settings.prefs_max_injected
            )
        except PrefsStoreUnavailable as exc:
            log.warning(
                "prefs load failed (injecting none): run_id=%s err=%r", run_id, exc
            )
        except Exception as exc:  # noqa: BLE001 — injection is best-effort:
            # a novel fault here must never kill an otherwise healthy chat
            # stream; the reply simply runs without the preferences block.
            log.warning(
                "prefs load raised unexpectedly (injecting none): "
                "run_id=%s err=%r",
                run_id,
                exc,
            )
        prefs_text = prefs_block(prefs)

    # Live frame channel + confirmation gate (openspec/changes/agent-feed-tools
    # D2/D5/D6): the sink lets frames — including the gate's mid-node
    # confirm prompt — reach the client during the approval pause; the gate
    # is wired whenever Redis is configured, and fails closed (mutating
    # calls close as denials) when it is not. Nothing auto-approves.
    run_queue: asyncio.Queue = asyncio.Queue()
    sink = RunSink(run_queue)
    redis = getattr(app.state, "redis", None)
    approvals: ApprovalGate | None = None
    if redis is not None:
        approvals = ApprovalGate(
            store=ApprovalStore(redis),
            ttl_seconds=settings.approval_ttl_seconds,
            sink=sink,
        )

    # Build the graph with the checkpointer if persistence is enabled.
    # Re-build per-request because the saver needs `thread_id` per-run.
    graph = build_graph(
        settings=settings,
        budget=app.state.budget,
        rag=rag,
        tools=tools,
        checkpointer=checkpointer,
        # The nodes must emit spans on the service's own tracer — not a
        # fresh global lookup, which would diverge in multi-app processes
        # (see build_graph's docstring).
        tracer=app.state.tracer,
        prefs_text=prefs_text,
        approvals=approvals,
    )

    graph_config: dict | None = None
    if checkpointer is not None:
        graph_config = {"configurable": {"thread_id": run_id}}

    # Opt-in LLM tracing (openspec/changes/agent-langfuse-tracing): the
    # callback handler rides the same config langGraph propagates into node
    # runnables, so every ChatOpenAI call site (planner structured/legacy,
    # executor llm/react, tool re-prompt) is captured without touching them.
    # Identity (spec "Traces carry run and user identity"): the run_id —
    # which doubles as the langGraph thread_id — is the Langfuse session,
    # and the Go-verified X-Nyx-User-Id is the Langfuse user, so a run is
    # replayable and filterable per user.
    langfuse_handler = get_langfuse_handler()
    if langfuse_handler is not None:
        graph_config = graph_config or {}
        graph_config.setdefault("callbacks", []).append(langfuse_handler)
        trace_metadata = graph_config.setdefault("metadata", {})
        # The handler reads these metadata keys (str values only);
        # `langfuse_session_id` lands as the trace's session id, not the
        # thread's. See CallbackHandler._build_trace_attributes.
        trace_metadata["langfuse_session_id"] = str(run_id)
        trace_metadata["langfuse_user_id"] = str(user_id)

    # Liveness heartbeat: while this stream owns the run, refresh
    # `last_heartbeat_at` so the stale-run sweeper can tell a live run
    # from a crash (openspec/changes/agent-stale-run-sweeper). Started
    # immediately before the try so the task can never leak: the only
    # awaits before this point are the two client-visible opening frames,
    # and a disconnect there exits the generator before the task exists.
    heartbeat_task: asyncio.Task[None] | None = None
    if run_metadata is not None and persist_run_state:
        # Session continuations skip the heartbeat: `agent.runs` has no
        # live row for this execution (terminal or absent), so touches
        # would be no-ops against a completed row or phantoms for a
        # session-only id (openspec/changes/agent-chat-history-ui).
        heartbeat_task = asyncio.create_task(
            _heartbeat_loop(
                run_metadata, run_id, settings.heartbeat_interval_seconds
            ),
            name=f"agent-heartbeat-{run_id}",
        )

    total_tokens = 0
    prompt_tokens: int | None = None
    completion_tokens: int | None = None
    error_code: str | None = None
    saw_done = False
    # Chat-history accumulation (openspec/changes/agent-chat-history-storage):
    # collect the assistant's text via every `delta` frame and every tool
    # invocation via `tool_call` frames. The final assistant message is
    # recorded after the run completes (best-effort; failures degrade to
    # warn-log and the stream is unaffected).
    assistant_text_parts: list[str] = []
    assistant_tool_calls: list[dict] = []
    assistant_steps = StepTrace()
    pump_task: asyncio.Task[None] | None = None
    try:
        # Live drain (agent-feed-tools D2): the pump drives the graph while
        # the drain forwards frames from the sink queue — the gate's
        # confirm prompt arrives mid-node, and an idle run gets SSE
        # keep-alive comments instead of a stalled connection.
        pump_task = asyncio.create_task(
            _pump_events(
                state,
                sink=sink,
                settings=settings,
                tracer=app.state.tracer,
                budget=app.state.budget,
                rag=rag,
                tools=tools,
                graph=graph,
                graph_config=graph_config,
                resumed=resumed,
            ),
            name=f"agent-pump-{run_id}",
        )
        async for item in _drain_run_queue(run_queue):
            if item is None:
                break
            kind, payload = item
            if kind == "comment":
                # SSE comment frame — protocol-legal, skipped by the
                # existing frontend parser.
                yield f": {payload}\n\n".encode()
                continue
            event = payload  # an ("event", Event) pair
            assistant_steps.fold(event.kind, event.payload)
            yield _sse_encode(event)
            # Track terminal totals for metadata persistence.
            if event.kind == "done":
                usage = event.payload.get("usage") or {}
                total_tokens = int(usage.get("total_tokens", 0))
                prompt_raw = usage.get("prompt_tokens")
                completion_raw = usage.get("completion_tokens")
                prompt_tokens = (
                    int(prompt_raw) if prompt_raw is not None else None
                )
                completion_tokens = (
                    int(completion_raw) if completion_raw is not None else None
                )
                saw_done = True
            elif event.kind == "error":
                error_code = event.payload.get("code")
            elif event.kind == "delta":
                text = event.payload.get("delta")
                if isinstance(text, str):
                    assistant_text_parts.append(text)
            elif event.kind == "tool_call":
                # Capture verbatim from the wire envelope so future replay
                # is byte-faithful (design.md D3).
                assistant_tool_calls.append(
                    {
                        "step_id": event.payload.get("step_id"),
                        "call_id": event.payload.get("call_id"),
                        "name": event.payload.get("name"),
                        "args": event.payload.get("args"),
                    }
                )

        # Drain ended (the pump closed the channel). Surface any pump
        # failure with the same semantics the pre-sink direct iteration
        # had — the exception propagates through the finally below.
        if pump_task.done() and pump_task.exception() is not None:
            raise pump_task.exception()
    finally:
        if pump_task is not None and not pump_task.done():
            pump_task.cancel()
            try:
                await pump_task
            except asyncio.CancelledError:
                pass
        # Stop the heartbeat before the terminal write so a late touch
        # cannot interleave with (or outlive) the terminal transition.
        if heartbeat_task is not None:
            heartbeat_task.cancel()
            try:
                await heartbeat_task
            except asyncio.CancelledError:
                pass

        # Terminal persistence must SURVIVE a client disconnect. The SPA
        # parser closes the stream on the `terminator` frame, Go tears the
        # upstream request down, and this generator gets cancelled at its
        # next await — which lands in here. Before this shield, a
        # fully-streamed answer could lose BOTH its assistant row and its
        # terminal run transition (observed live 2026-10-14: chat_messages
        # kept only the user row and agent.runs stayed 'running' for a
        # completed reply). Run the writes in a detached task and await it
        # through `asyncio.shield`: cancellation here leaves the task
        # running to completion in the background.
        async def _persist_terminal() -> None:
            # Session continuations skip the metadata block: agent.runs
            # owns per-RUN rows, and this execution has none to transition
            # (a terminal row must not be mutated back, and a phantom row
            # must not be minted for a session-only id —
            # openspec/changes/agent-chat-history-ui).
            if run_metadata is not None and persist_run_state:
                try:
                    if error_code is not None and error_code != "agent_unavailable":
                        await run_metadata.mark_failed(run_id, error_code)
                    elif error_code is None and saw_done:
                        await run_metadata.mark_completed(run_id, total_tokens)
                    # error_code == "agent_unavailable", or a stream closed
                    # before `done` (client disconnect mid-run) → leave the
                    # row `running`: the run is still resumable inside the
                    # window, and the stale-run sweeper reaps it if nobody
                    # comes back (test-review C1, 2026-10-14).
                except RunMetadataUnavailable as exc:
                    log.warning(
                        "metadata terminal write failed (non-fatal): "
                        "run_id=%s err=%r",
                        run_id,
                        exc,
                    )

            # Chat-history: record the assistant message once the run
            # reaches a terminal state (design.md D6/D7). Only when the run
            # reached `done`; partial-failure runs leave no assistant row —
            # replay should show the failure, not a half-answer.
            if (
                chat_store is not None
                and saw_done
                and (assistant_text_parts or assistant_tool_calls)
            ):
                try:
                    await chat_store.record_message(
                        run_id,
                        user_id,
                        role="assistant",
                        content="".join(assistant_text_parts),
                        tool_calls=assistant_tool_calls or None,
                        steps=assistant_steps.to_list(),
                        tokens=total_tokens,
                        prompt_tokens=prompt_tokens,
                        completion_tokens=completion_tokens,
                    )
                    # Stamp the session's ended_at so the sidebar can
                    # render a "finished" state instead of "live".
                    await chat_store.end_session(run_id, user_id)
                except ChatStoreUnavailable as exc:
                    log.warning(
                        "chat_store assistant record failed (non-fatal): "
                        "run_id=%s err=%r",
                        run_id,
                        exc,
                    )

        finalize_task = asyncio.create_task(
            _persist_terminal(), name=f"agent-finalize-{run_id}"
        )
        # Module-level strong ref so the task is never GC'd before it
        # finishes (asyncio only holds weak refs).
        _FINALIZE_TASKS.add(finalize_task)
        finalize_task.add_done_callback(_FINALIZE_TASKS.discard)
        try:
            await asyncio.shield(finalize_task)
        except asyncio.CancelledError:
            log.info(
                "client disconnected during terminal persistence; the "
                "shielded write continues in the background (run_id=%s)",
                run_id,
            )
            raise

        # Preference extraction (openspec/changes/agent-user-preferences,
        # design D2/D4): fire-and-forget AFTER the reply is finalized — it
        # never delays, blocks, or alters the SSE stream. Spawned only on a
        # completed run (`saw_done`) past the cheap gate; the tracked task
        # holds no reference to this generator, so a later disconnect
        # cannot cancel it (and a disconnect during the shield above skips
        # the spawn entirely — acceptable per D2: the next
        # preference-bearing turn re-extracts; keys are stable).
        if settings.prefs_enabled and saw_done and prefs_store is not None:
            last_user_text = next(
                (
                    (m.content or "")
                    for m in reversed(chat_req.messages)
                    if m.role == "user"
                ),
                "",
            )
            if last_user_text and looks_like_preference(last_user_text):
                prefs_task = asyncio.create_task(
                    _extract_and_store_prefs(
                        prefs_store, user_id, run_id, last_user_text, settings
                    ),
                    name=f"agent-prefs-{run_id}",
                )
                # Module-level strong ref (mirrors `_FINALIZE_TASKS`):
                # asyncio holds only weak refs, so without this the task
                # could be GC'd mid-write after a disconnect teardown.
                _PREFS_TASKS.add(prefs_task)
                prefs_task.add_done_callback(_PREFS_TASKS.discard)

        if (
            _should_refine_title(
                saw_done=saw_done,
                title_written=title_written,
                chat_store=chat_store,
            )
            and chat_store is not None
        ):
            _spawn_title_refinement(
                chat_store,
                user_id=user_id,
                run_id=run_id,
                user_text=next(
                    (
                        (m.content or "")
                        for m in chat_req.messages
                        if m.role == "user"
                    ),
                    "",
                ),
                assistant_text="".join(assistant_text_parts),
                settings=settings,
            )

    # Frontend parser triggers its own terminator on `data: [DONE]`. Old clients
    # that listen on `event: terminator` still work because we emit that event too.
    yield b"data: [DONE]\n\n"


async def _extract_and_store_prefs(
    prefs_store: PrefsStore,
    user_id: int,
    run_id: str,
    text: str,
    settings: Settings,
) -> None:
    """Detached preference extraction + upsert for one turn (design D2/D4).

    Spawned from `_stream_agent_run`'s finally (post-`done`) via
    `asyncio.create_task` and tracked in `_PREFS_TASKS`. Contract:

    - Never touches the SSE stream: no frame ever carries extraction state,
      and the reply has already reached the client when this runs.
    - NEVER consults the budget guard (design D4, explicit): the budget
      prices *user-visible* model work, and extraction is an internal
      byproduct bounded structurally to one small call per
      preference-bearing turn. Charging it to the user's cap would make
      prefs silently accelerate budget exhaustion — a UX trap.
    - Failure-isolated: `extract_preferences` returns `[]` on any LLM or
      parse failure by contract; an upsert fault (or anything else) is
      warn-logged here and ends the task. Nothing is retried within the
      run (design D2).
    """
    try:
        prefs = await extract_preferences(text, settings=settings)
        for p in prefs:
            await prefs_store.upsert_pref(
                user_id, p.key, p.value, source_session_id=run_id
            )
    except Exception as exc:  # noqa: BLE001 — warn-and-continue (D2)
        log.warning(
            "preference extraction/storage failed (non-fatal): "
            "run_id=%s user_id=%s err=%r",
            run_id,
            user_id,
            exc,
        )


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
    return f"event: {event.kind}\ndata: {data}\n\n".encode()


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