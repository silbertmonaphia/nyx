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

import json
import logging
import uuid
from contextlib import asynccontextmanager
from typing import AsyncIterator

from fastapi import FastAPI, HTTPException, Request, Response
from fastapi.responses import StreamingResponse
from prometheus_client import generate_latest, CONTENT_TYPE_LATEST
from redis.asyncio import Redis

from .budget import BudgetGuard
from .config import Settings, get_settings
from .graph import run_agent_stream
from .observability import setup_observability, shutdown_observability
from .rag_client import RagClient
from .state import new_state
from .tools_client import ToolsClient
from .types import ChatRequest, Event

log = logging.getLogger("nyx.agent")


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

    log.info(
        "agent_py started",
        extra={
            "port": settings.port,
            "metrics_port": settings.metrics_port,
            "llm_model": settings.llm_model,
            "planner_model": settings.planner_model,
            "budget_cap": settings.budget_per_user_per_hour,
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