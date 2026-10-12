"""OTel + Prometheus setup helpers.

Tracer provider is built once at app startup via `setup_observability(settings)`.
Prometheus counters (`agent_runs_total`, etc.) are defined here and incremented at the
call site that emits the corresponding event.
"""

from __future__ import annotations

import requests
from opentelemetry import trace
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from prometheus_client import Counter

from .config import Settings

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