"""OTel + Prometheus setup helpers.

Tracer provider is built once at app startup via `setup_observability(settings)`.
Prometheus counters (`agent_runs_total`, etc.) are defined here and incremented at the
call site that emits the corresponding event.

Langfuse LLM tracing lives here too (openspec/changes/agent-langfuse-tracing):
`setup_langfuse(settings)` builds an opt-in Langfuse client on its OWN isolated
TracerProvider — deliberately never registered globally, because the global provider
belongs to the Jaeger export pipeline (see that function's docstring).
"""

from __future__ import annotations

import logging
import re
from contextlib import contextmanager
from contextvars import ContextVar
from typing import Any, Iterator, Optional

import requests
from opentelemetry.context import Context
from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SpanExporter
from prometheus_client import Counter

from .config import Settings

logger = logging.getLogger(__name__)

_TRACER_NAME = "nyx.agent"

# Prometheus counters — names match what the Go agent emitted so existing dashboards
# (label-set, rate() expressions) keep working across the runtime swap.

agent_runs_total = Counter(
    "agent_runs_total",
    "Number of agent runs, labelled by kind (planner|executor|react) and status (ok|failed|budget_exceeded|budget_unavailable).",
    labelnames=("kind", "status"),
)

agent_steps_total = Counter(
    "agent_steps_total",
    "Number of agent steps, labelled by kind (rag|tool|llm|react) and status (ok|failed).",
    labelnames=("kind", "status"),
)

agent_tool_calls_total = Counter(
    "agent_tool_calls_total",
    "Number of tool calls dispatched, labelled by tool name and ok flag.",
    labelnames=("tool", "ok"),
)

agent_tokens_total = Counter(
    "agent_tokens_total",
    "Tokens consumed, labelled by role (planner|executor|rag|react) and model id.",
    labelnames=("role", "model"),
)

agent_planner_parse_failures_total = Counter(
    "agent_planner_parse_failures_total",
    "Planner Pydantic validation failures, labelled by retry outcome "
    "(ok = recovered on retry; exhausted = terminated with plan_invalid).",
    labelnames=("retry",),
)

agent_tool_retries_total = Counter(
    "agent_tool_retries_total",
    "Tool dispatch retries, labelled by tool name and reason (tool_arg_invalid | tool_execution_transient).",
    labelnames=("tool", "reason"),
)

_provider: TracerProvider | None = None


def setup_observability(settings: Settings) -> trace.Tracer:
    """Build the OTel tracer provider once. Idempotent — safe to call from FastAPI lifespan."""
    global _provider
    if _provider is not None:
        return trace.get_tracer(_TRACER_NAME)

    resource = Resource.create({"service.name": settings.otel_service_name})
    # Span export must never traverse an HTTP proxy: the endpoint is always
    # internal (Jaeger on the host, or in-network in compose), and a proxy
    # that can't reach it returns 502s — which the exporter then retries
    # forever ("Transient error Bad Gateway encountered while exporting
    # spans batch"). trust_env=False ignores HTTP_PROXY/NO_PROXY entirely
    # for this session, so export behaviour is deterministic regardless of
    # the operator's proxy config. (Verified against exporter 1.45.1: the
    # `session` kwarg is documented and used for the POST.)
    export_session = requests.Session()
    export_session.trust_env = False
    exporter = OTLPSpanExporter(
        endpoint=f"{settings.otel_endpoint}/v1/traces", session=export_session
    )
    provider = TracerProvider(resource=resource)
    provider.add_span_processor(BatchSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    _provider = provider
    return trace.get_tracer(_TRACER_NAME)


def get_tracer() -> trace.Tracer:
    return trace.get_tracer(_TRACER_NAME)


def shutdown_observability() -> None:
    global _provider
    if _provider is not None:
        _provider.shutdown()
        _provider = None


# ---------------------------------------------------------------------------
# Langfuse LLM tracing (opt-in) — openspec/changes/agent-langfuse-tracing.
#
# Export contract (spec `agent-llm-tracing`):
#   - Disabled (default): nothing is constructed, nothing leaves the process.
#   - Enabled: prompts/replies/usage ride the LangChain callback handler on the
#     graph-invocation config; plan steps + tool calls ride `trace_observation`.
#   - Delivery is best-effort: every helper here logs-and-continues on failure;
#     no exception from trace code may propagate into a run.
#   - Credential-shaped strings are scrubbed twice: on data we set through SDK
#     APIs (`mask`, which also covers handler-captured payloads) and again at
#     the export stage (`mask_otel_spans`, a backstop over the final OTel
#     batch). Scrubbing is idempotent, so applying both is safe.
# ---------------------------------------------------------------------------

# Credential shapes we refuse to store: `Authorization: Bearer <token>`
# headers (the agent forwards the caller's bearer to the MCP/RAG bridges),
# raw JWTs, and API-key-shaped tokens. Kept deliberately small and anchored —
# a blocklist regex that eats arbitrary text is worse than no masking.
_CREDENTIAL_PATTERNS = (
    re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}"),
    re.compile(r"\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}"),
    re.compile(r"\b(?:sk|pk)-lf-[A-Za-z0-9-]{4,}\b"),
    # OpenAI-style keys (`sk-...`, `sk-proj-...`): 16+ token chars after the
    # prefix, hyphens/underscores allowed in the body.
    re.compile(r"\bsk-[A-Za-z0-9][A-Za-z0-9_-]{15,}\b"),
)

_REDACTED = "[REDACTED]"


def scrub_text(text: str) -> str:
    """Replace credential-shaped substrings with a redaction marker.

    Safe on JSON-encoded strings: the marker contains no quotes, brackets are
    legal inside JSON string values, and the patterns only match token shapes.
    """
    for pattern in _CREDENTIAL_PATTERNS:
        text = pattern.sub(_REDACTED, text)
    return text


def scrub_value(value: Any) -> Any:
    """Recursively scrub strings inside dicts/lists/tuples; other types pass through."""
    if isinstance(value, str):
        return scrub_text(value)
    if isinstance(value, dict):
        return {k: scrub_value(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [scrub_value(v) for v in value] if isinstance(value, list) else tuple(
            scrub_value(v) for v in value
        )
    return value


def mask_langfuse_data(*, data: Any, **kwargs: Any) -> Any:
    """`mask=` hook: scrub data the SDK is about to attach to an observation."""
    return scrub_value(data)


def _scrub_otel_attribute(value: Any) -> Any:
    """Scrub one OTel attribute value (scalar, or homogeneous scalar sequence)."""
    if isinstance(value, str):
        return scrub_text(value)
    if isinstance(value, (list, tuple)):
        return type(value)(_scrub_otel_attribute(v) for v in value)
    return value


def mask_langfuse_otel_spans(*, params: Any) -> Optional[Any]:
    """`mask_otel_spans=` hook: export-stage scrub of the final OTel batch.

    Runs on the span-processor worker thread — kept synchronous and cheap
    (a handful of regexes per string attribute). Returns None when nothing
    changed, so clean batches skip patching entirely.
    """
    from langfuse.types import MaskOtelSpansResult, OtelSpanPatch

    patches: dict[Any, Any] = {}
    for identifier, span in params.spans.items():
        changed = {}
        for key, value in span.attributes.items():
            scrubbed = _scrub_otel_attribute(value)
            if scrubbed != value:
                changed[key] = scrubbed
        if changed:
            patches[identifier] = OtelSpanPatch(set_attributes=changed)
    if not patches:
        return None
    return MaskOtelSpansResult(span_patches=patches)


_langfuse_client: Any = None
_langfuse_provider: TracerProvider | None = None
_langfuse_handler: Any = None


def _build_langfuse_span_exporter(settings: Settings) -> SpanExporter:
    """OTLP/HTTP exporter to `<host>/api/public/otel/v1/traces`.

    Mirrors the exporter the langfuse SDK would build itself (same endpoint,
    same basic-auth + SDK headers), with one deliberate difference: the
    requests session has `trust_env=False`, so span export never traverses
    an HTTP proxy — the exact failure class documented on the Jaeger exporter
    above ("Transient error Bad Gateway encountered while exporting spans
    batch" forever). Deterministic regardless of the operator's proxy config.
    """
    import base64

    from langfuse import __version__ as langfuse_version

    auth = base64.b64encode(
        f"{settings.langfuse_public_key}:"
        f"{settings.langfuse_secret_key.get_secret_value()}".encode("utf-8")
    ).decode("ascii")
    export_session = requests.Session()
    export_session.trust_env = False
    return OTLPSpanExporter(
        endpoint=f"{settings.langfuse_host}/api/public/otel/v1/traces",
        headers={
            "Authorization": f"Basic {auth}",
            "x-langfuse-sdk-name": "python",
            "x-langfuse-sdk-version": langfuse_version,
            "x-langfuse-public-key": settings.langfuse_public_key,
        },
        session=export_session,
    )


def setup_langfuse(
    settings: Settings, span_exporter: Optional[SpanExporter] = None
) -> Any:
    """Build the Langfuse client once. Idempotent — safe from FastAPI lifespan.

    Returns the client, or None when tracing is disabled. The client gets its
    OWN TracerProvider, which is never registered globally: `setup_observability`
    already installed the Jaeger provider as the global one, and the langfuse
    SDK attaches its span processor to whatever provider it is handed — sharing
    the global provider would (a) leak `langfuse-sdk`-scope spans into Jaeger
    exports, (b) make behavior depend on setup order. The SDK builds its own
    OTLP exporter against `base_url` unless `span_exporter` is injected (test
    seam only).

    The Langfuse SDK is imported lazily so the disabled path pays no import
    cost and a broken SDK install cannot take down the agent's boot.
    """
    global _langfuse_client, _langfuse_provider
    if not settings.langfuse_enabled:
        return None
    if _langfuse_client is not None:
        return _langfuse_client

    try:
        from langfuse import Langfuse

        if span_exporter is None:
            span_exporter = _build_langfuse_span_exporter(settings)

        provider = TracerProvider(
            resource=Resource.create({"service.name": settings.otel_service_name})
        )
        client = Langfuse(
            public_key=settings.langfuse_public_key,
            secret_key=settings.langfuse_secret_key.get_secret_value(),
            base_url=settings.langfuse_host,
            tracer_provider=provider,
            mask=mask_langfuse_data,
            mask_otel_spans=mask_langfuse_otel_spans,
            span_exporter=span_exporter,
        )
    except Exception:
        # D6: an SDK-side failure degrades to tracing-off — it must not
        # abort agent boot. Genuine misconfiguration (enabled without keys)
        # is refused earlier by the settings validator, so reaching this
        # except means the SDK itself failed, not the operator's config.
        logger.warning(
            "langfuse tracing could not be initialized; continuing without tracing",
            exc_info=True,
        )
        return None
    _langfuse_client = client
    _langfuse_provider = provider
    logger.info("langfuse tracing enabled (host=%s)", settings.langfuse_host)
    return client


def get_langfuse_handler() -> Any:
    """LangChain callback handler bound to the configured client, or None.

    The handler resolves its client through the SDK's per-public-key registry,
    so the isolated `tracer_provider` from `setup_langfuse` is preserved.
    Construction failures log at warn and return None (D6): this runs on
    every run's request path, and an SDK error here must mean "no tracing
    for this run", never a failed run.
    """
    global _langfuse_handler
    if _langfuse_client is None:
        return None
    if _langfuse_handler is None:
        try:
            from langfuse.langchain import CallbackHandler

            _langfuse_handler = CallbackHandler()
        except Exception:
            logger.warning(
                "langfuse callback handler unavailable; runs continue untraced",
                exc_info=True,
            )
            return None
    return _langfuse_handler


# Parent observation for NESTED trace_observation calls (step → tool_call).
# ContextVar, not a plain stack: concurrent runs in one process must not
# see each other's parents.
_langfuse_parent: ContextVar[Optional[str]] = ContextVar(
    "nyx_langfuse_parent", default=None
)

# Trace id for the current run's Langfuse tree — captured once at the first
# observation (where the handler's span is innermost) and reused by nested
# ones, so nested calls never have to consult the ambient OTel context.
_langfuse_trace_id: ContextVar[Optional[str]] = ContextVar(
    "nyx_langfuse_trace_id", default=None
)

# Bound on the SDK shutdown/flush wait (see shutdown_langfuse). Long enough
# for a healthy flush of a full run's batches; short enough that a wedged
# SDK join cannot meaningfully stall service teardown.
_LANGFUSE_SHUTDOWN_TIMEOUT_SECONDS = 5.0

# Jaeger-side parent for OUR spans (see jaeger_span).
_jaeger_parent: ContextVar[Any] = ContextVar("nyx_jaeger_parent", default=None)


@contextmanager
def jaeger_span(tracer: trace.Tracer, name: str) -> Iterator[trace.Span]:
    """`tracer.start_as_current_span` with Jaeger-tracked parenting.

    Two-layer rule, chosen so the Jaeger tree is IDENTICAL whether tracing
    is enabled or disabled (and identical to what the pre-tracing stack
    produced):
      - nested under another jaeger_span → explicit parent from our own
        ContextVar (deterministic, independent of ambient context);
      - otherwise → ambient parenting, exactly like the plain SDK call
        (e.g. under the ASGI 'POST /agent/run' server span) — EXCEPT when
        the innermost ambient span is a langfuse SDK span, which must never
        become a Jaeger parent (it never reaches the Jaeger exporter, so
        children would render as orphaned roots). Verified against SDK
        4.17.0: the callback handler does not attach its spans to the
        ambient OTel context, so this fallback is defensive only.
    """
    parent = _jaeger_parent.get()
    ctx: Optional[Context] = None
    if parent is not None:
        ctx = trace.set_span_in_context(parent)
    else:
        current = trace.get_current_span()
        # `get_current_span()` returns a NonRecordingSpan (no
        # instrumentation_scope) when nothing is active, which
        # `is_langfuse_span` cannot introspect — guard before probing.
        if hasattr(current, "instrumentation_scope"):
            from langfuse import is_langfuse_span

            if is_langfuse_span(current):
                ctx = Context()
    kwargs = {"context": ctx} if ctx is not None else {}
    with tracer.start_as_current_span(name, **kwargs) as span:
        token = _jaeger_parent.set(span)
        try:
            yield span
        finally:
            _jaeger_parent.reset(token)


@contextmanager
def trace_observation(
    name: str,
    *,
    input: Any = None,
    metadata: Optional[dict[str, Any]] = None,
) -> Iterator[Any]:
    """Langfuse span around one unit of work; no-op when tracing is disabled.

    Uses the NON-context creation API (`start_observation` + explicit
    parenting + `end()`) on purpose. `start_as_current_observation` would set
    the observation as the ambient OTel "current span" — a provider-agnostic
    slot — so any Jaeger span created inside the wrapped block (e.g.
    `agent.planner.structured`, `agent.budget`) would silently re-parent onto
    a Langfuse-only span id that never reaches Jaeger, orphaning the Jaeger
    tree whenever tracing is enabled (found in review; proven by a
    disabled-vs-enabled parent-id probe). With this shape the shared context
    is never touched: Jaeger spans keep their Jaeger parents, and Langfuse
    observations are wired into the Langfuse trace through explicit
    `trace_context` (the handler's active span) + our own parent ContextVar.

    Skips (yields None) when no Langfuse trace is active in the current
    context — a detached observation would strand a one-span trace. All
    failure modes log-and-continue: the wrapped work must run regardless,
    and exceptions from the body propagate unchanged.
    """
    client = _langfuse_client
    if client is None:
        yield None
        return
    try:
        trace_id = _langfuse_trace_id.get()
        parent_id = _langfuse_parent.get()
        if trace_id is None:
            # First (outermost) observation in this context. The SDK's
            # get_current_trace_id/get_current_observation_id resolve from
            # the langfuse SDK's OWN context (the callback handler's active
            # span) — verified against 4.17.0: the ambient OTel span here
            # is a foreign (non-langfuse) span, while these methods still
            # return the run's trace/observation. When no langfuse span is
            # active they return None, which is the skip condition: a
            # detached observation would strand a one-span trace.
            trace_id = client.get_current_trace_id()
            if not trace_id:
                yield None
                return
            if parent_id is None:
                parent_id = client.get_current_observation_id()
        trace_context: dict[str, str] = {"trace_id": trace_id}
        if parent_id:
            trace_context["parent_span_id"] = parent_id
        observation = client.start_observation(
            as_type="span", name=name, trace_context=trace_context
        )
    except Exception:  # pragma: no cover - defensive: SDK internals changed
        logger.warning("langfuse observation %s could not be opened", name, exc_info=True)
        yield None
        return
    trace_token = _langfuse_trace_id.set(trace_id)
    parent_token = _langfuse_parent.set(observation.id)
    try:
        fields: dict[str, Any] = {}
        if input is not None:
            fields["input"] = input
        if metadata is not None:
            fields["metadata"] = metadata
        if fields:
            update_observation(observation, **fields)
        yield observation
    finally:
        _langfuse_parent.reset(parent_token)
        _langfuse_trace_id.reset(trace_token)
        try:
            observation.end()
        except Exception:  # pragma: no cover - defensive: SDK internals changed
            logger.warning("langfuse observation %s could not be closed", name, exc_info=True)


def update_observation(observation: Any, **fields: Any) -> None:
    """Best-effort `observation.update(**fields)` with scrubbing.

    No-op on None (tracing disabled). Any SDK-side failure logs at warn —
    trace updates must never propagate into the wrapped run.
    """
    if observation is None:
        return
    try:
        observation.update(**{k: scrub_value(v) for k, v in fields.items()})
    except Exception:
        logger.warning("langfuse observation update failed", exc_info=True)


def shutdown_langfuse() -> None:
    """Flush + shut down the tracing client. Idempotent, never raises,
    and never blocks unboundedly.

    The langfuse SDK's `client.shutdown()` can wedge: it calls
    `resource_manager.flush()`, which does an UNBOUNDED `queue.join()` on
    the SDK's ingestion queues. In a process that constructed several
    clients (every lifespan boot constructs one), a queue can end up
    drained-but-not-joined with no live consumer — observed as a full
    pytest hang at fixture teardown once the dev `.env` enabled tracing
    (faulthandler stack: shutdown_langfuse → client.shutdown →
    resource_manager.flush → queue.join, exit 124 under `timeout`).
    The SDK's sibling threads are daemons, so bounding the wait and
    abandoning a wedged shutdown is safe: the interpreter still exits,
    and the worst case is the last un-exported batch being dropped —
    exactly the documented best-effort delivery contract (spec
    `agent-llm-tracing` — "Trace delivery is best-effort").
    """
    global _langfuse_client, _langfuse_provider, _langfuse_handler
    client = _langfuse_client
    # Reset state first so a wedged SDK thread can never make shutdown
    # non-idempotent or block a re-entrant call.
    _langfuse_client = None
    _langfuse_provider = None
    _langfuse_handler = None
    if client is None:
        return

    import threading

    finished = threading.Event()

    def _do_shutdown() -> None:
        try:
            client.shutdown()
        except Exception:
            logger.warning("langfuse shutdown failed", exc_info=True)
        finally:
            finished.set()

    worker = threading.Thread(
        target=_do_shutdown, name="nyx-langfuse-shutdown", daemon=True
    )
    worker.start()
    if not finished.wait(timeout=_LANGFUSE_SHUTDOWN_TIMEOUT_SECONDS):
        logger.warning(
            "langfuse shutdown did not complete within %.0fs; abandoning the "
            "SDK's flush (traces may be dropped — best-effort delivery)",
            _LANGFUSE_SHUTDOWN_TIMEOUT_SECONDS,
        )