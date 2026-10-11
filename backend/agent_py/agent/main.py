"""FastAPI app factory for the Python agent service.

Routes:
  - POST /agent/run  — accepts ChatRequest, streams the SSE envelope byte-for-byte to the
    Go chat handler's response. The Go side then proxies these bytes to the browser
    unchanged. Bearer JWT verified; user id read from the token.
  - GET  /healthz     — 200 with `{"status":"ok"}` when Redis is reachable; 503 otherwise.
  - GET  /metrics     — Prometheus counters.
"""

from __future__ import annotations

import json
import logging
import uuid
from contextlib import asynccontextmanager
from typing import AsyncIterator

from fastapi import FastAPI, HTTPException, Request, Response
from fastapi.responses import StreamingResponse
import jwt
from jwt.exceptions import PyJWTError
from prometheus_client import generate_latest, CONTENT_TYPE_LATEST
from redis.asyncio import Redis

from .budget import BudgetGuard
from .config import Settings, get_settings
from .graph import run_agent_stream
from .observability import setup_observability, shutdown_observability
from .rag_client import RagClient
from .state import new_state
from .tools_client import ToolsClient
from .types import ChatRequest, Event, TerminatorEvent

log = logging.getLogger("nyx.agent")


@asynccontextmanager
async def lifespan(app: FastAPI):
    settings = get_settings()

    # S-1 defense in depth: refuse startup when JWT secret is unset and
    # the operator hasn't explicitly opted in to unsigned mode. This
    # closes the budget-DoS hole (a forged JWT could drain another user's
    # hourly counter before Go's downstream validation rejected the work).
    if not settings.jwt_secret and not settings.allow_unverified_jwt:
        raise RuntimeError(
            "refusing to start: PYTHON_AGENT_JWT_SECRET is unset and "
            "PYTHON_AGENT_ALLOW_UNVERIFIED_JWT is not 'true'. Set the secret "
            "for production, or opt in explicitly for local dev."
        )

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

    log.info(
        "agent_py started",
        extra={
            "port": settings.port,
            "metrics_port": settings.metrics_port,
            "llm_model": settings.llm_model,
            "planner_model": settings.planner_model,
            "budget_cap": settings.budget_per_user_per_hour,
            "jwt_verified": bool(settings.jwt_secret),
        },
    )
    try:
        yield
    finally:
        shutdown_observability()
        await redis.aclose()


def create_app(settings: Settings | None = None) -> FastAPI:
    settings = settings or get_settings()
    app = FastAPI(title="nyx-agent", lifespan=lifespan)

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
        # Auth: Bearer JWT.
        auth = req.headers.get("authorization", "")
        if not auth.startswith("Bearer "):
            raise HTTPException(status_code=401, detail="missing bearer token")
        bearer_token = auth.removeprefix("Bearer ").strip()
        user_id = _user_id_from_token(bearer_token, settings)
        if user_id is None:
            raise HTTPException(status_code=401, detail="invalid bearer token")

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
    """
    state = new_state(
        user_id=user_id,
        run_id=str(uuid.uuid4()),
        request_id=request_id,
        messages=chat_req.messages,
        model=chat_req.model,
    )

    # CRITICAL-3: thread the user's bearer token into the RAG + tool clients.
    rag = RagClient(base_url=settings.rag_bridge_url, bearer_token=bearer_token)
    tools = ToolsClient(base_url=settings.mcp_url, bearer_token=bearer_token)

    async for event in run_agent_stream(
        state,
        settings=settings,
        tracer=app.state.tracer,
        budget=app.state.budget,
        rag=rag,
        tools=tools,
    ):
        yield _sse_encode(event)

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
      - note          → {text, provider}
      - done          → {usage: {prompt_tokens, completion_tokens, total_tokens}}
      - error         → {error, code, request_id}
      - terminator    → {reason}
    """
    data = json.dumps(event.payload, separators=(",", ":"))
    return f"event: {event.kind}\ndata: {data}\n\n".encode("utf-8")


def _user_id_from_token(token: str, settings: Settings) -> int | None:
    """Decode the JWT, verifying signature unless explicitly opted out.

    Modes (S-1 fix):
      - With `jwt_secret` set: full HMAC verification + optional `aud`/`iss`
        checks (matches Go's `auth.TokenService` semantics).
      - Without `jwt_secret`: the service MUST be started with
        `PYTHON_AGENT_ALLOW_UNVERIFIED_JWT=true` (checked at startup below);
        otherwise the lifespan refuses to boot. This closes the budget-DoS
        hole where a forged JWT could drain another user's hourly budget
        before Go's downstream validation rejected the work.
    """
    decode_options: dict = {}
    if settings.jwt_audience:
        decode_options["audience"] = settings.jwt_audience
    if settings.jwt_issuer:
        decode_options["issuer"] = settings.jwt_issuer

    if not settings.jwt_secret:
        if not settings.allow_unverified_jwt:
            # Defense in depth: the lifespan startup check would have
            # already aborted, but a programmer who called this function
            # directly gets a clear failure.
            raise RuntimeError(
                    "JWT secret unset and allow_unverified_jwt=false; "
                    "set PYTHON_AGENT_JWT_SECRET or opt in via "
                    "PYTHON_AGENT_ALLOW_UNVERIFIED_JWT=true"
            )
        decode_options["verify_signature"] = False

    try:
        if settings.jwt_secret:
            claims = jwt.decode(
                token,
                key=settings.jwt_secret,
                algorithms=[settings.jwt_algorithm],
                options=decode_options,
            )
        else:
            claims = jwt.decode(token, options=decode_options)
    except PyJWTError:
        return None
    sub = claims.get("sub") or claims.get("user_id")
    if sub is None:
        return None
    try:
        return int(sub)
    except (TypeError, ValueError):
        return None


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