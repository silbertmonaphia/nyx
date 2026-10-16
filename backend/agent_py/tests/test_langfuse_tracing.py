"""Unit tests for the Langfuse tracing seam (openspec/changes/agent-langfuse-tracing).

Covers the spec `agent-llm-tracing` requirements that are unit-testable
in-process: opt-in gating (nothing constructed when disabled), provider
isolation (the global Jaeger OTel provider is never touched), lifecycle
(idempotent setup, flush-on-shutdown), and credential scrubbing (both the
SDK-side `mask` hook and the export-stage `mask_otel_spans` hook).

End-to-end behavior (trace trees in a live instance) is covered by the
Playwright spec frontend/tests/e2e/chat-langfuse-tracing.spec.ts and the
stub-LLM integration tests in the same directory as this file.
"""

from __future__ import annotations

import logging
import re
import uuid

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from agent import observability
from agent.config import Settings
from agent.main import configure_logging
from agent.observability import (
    mask_langfuse_data,
    mask_langfuse_otel_spans,
    scrub_text,
    scrub_value,
    setup_langfuse,
    shutdown_langfuse,
    trace_observation,
)

pytestmark = pytest.mark.usefixtures("_langfuse_cleanup")


@pytest.fixture
def _langfuse_cleanup():
    yield
    shutdown_langfuse()
    # The SDK keeps a process-wide registry keyed by public key and does NOT
    # de-register on shutdown; without this, later tests' `get_client()`
    # would see >=2 instances and silently disable tracing.
    try:
        from langfuse._client.resource_manager import LangfuseResourceManager

        LangfuseResourceManager._instances.clear()
    except Exception:  # pragma: no cover - SDK internals moved
        pass


def _settings(**overrides) -> Settings:
    """Enabled-settings with a unique public key per test.

    Unique keys matter: the SDK keeps a process-wide registry keyed by
    public key, so reusing one across tests would let a previous test's
    (shut-down) manager leak into the next lookup.
    """
    key_suffix = uuid.uuid4().hex[:8]
    base = dict(
        _env_file=None,
        llm_base_url="http://vllm:8000/v1",
        langfuse_enabled=True,
        langfuse_public_key=f"pk-lf-test-{key_suffix}",
        langfuse_secret_key=f"sk-lf-test-{key_suffix}",
        # Unreachable host: the injected in-memory exporter below means no
        # real network call should ever happen; the port guards against it.
        langfuse_host="http://127.0.0.1:9",
    )
    base.update(overrides)
    return Settings(**base)


# ---------------------------------------------------------------------------
# Opt-in gating and lifecycle
# ---------------------------------------------------------------------------


def test_disabled_settings_construct_nothing():
    """Spec "Tracing is opt-in and off by default": disabled → no client."""
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.langfuse_enabled is False
    assert setup_langfuse(s) is None
    assert observability._langfuse_client is None
    assert observability._langfuse_provider is None
    assert observability.get_langfuse_handler() is None


def test_enabled_setup_uses_isolated_provider():
    """The langfuse client must get its OWN TracerProvider — the global one
    belongs to the Jaeger export pipeline (design D2)."""
    global_before = trace.get_tracer_provider()
    exporter = InMemorySpanExporter()

    client = setup_langfuse(_settings(), span_exporter=exporter)

    assert client is not None
    assert observability._langfuse_provider is not None
    # Global provider identity unchanged; langfuse provider is NOT the global.
    assert trace.get_tracer_provider() is global_before
    assert observability._langfuse_provider is not global_before

    # A span created through the client's own API lands in the isolated
    # provider's exporter — proving the wiring end to end.
    with client.start_as_current_observation(as_type="span", name="probe"):
        pass
    client.flush()
    span_names = [s.name for s in exporter.get_finished_spans()]
    assert "probe" in span_names


def test_setup_langfuse_is_idempotent():
    exporter = InMemorySpanExporter()
    first = setup_langfuse(_settings(), span_exporter=exporter)
    second = setup_langfuse(_settings(), span_exporter=exporter)
    assert first is second


def test_shutdown_flushes_and_resets():
    exporter = InMemorySpanExporter()
    client = setup_langfuse(_settings(), span_exporter=exporter)
    with client.start_as_current_observation(as_type="span", name="pending"):
        pass
    shutdown_langfuse()
    assert observability._langfuse_client is None
    assert observability._langfuse_provider is None
    # Shutdown flushed the span that had not been exported yet.
    assert "pending" in [s.name for s in exporter.get_finished_spans()]
    # Idempotent: a second shutdown with no client is a no-op.
    shutdown_langfuse()


def test_handler_returns_none_when_disabled_and_instance_when_enabled():
    assert observability.get_langfuse_handler() is None
    setup_langfuse(_settings(), span_exporter=InMemorySpanExporter())
    handler = observability.get_langfuse_handler()
    assert handler is not None
    # Same handler on subsequent calls.
    assert observability.get_langfuse_handler() is handler


def test_trace_observation_is_noop_when_disabled():
    """Best-effort contract: with tracing off the helper yields None and the
    wrapped body runs — and an exception from the body still propagates."""
    with trace_observation("noop", input={"a": 1}) as span:
        assert span is None
    with pytest.raises(RuntimeError):
        with trace_observation("noop"):
            raise RuntimeError("body errors stay untouched")


def test_trace_observation_records_span_when_enabled():
    """Inside an active langfuse span the helper records an observation
    nested under it (explicit parenting, not ambient OTel context)."""
    exporter = InMemorySpanExporter()
    client = setup_langfuse(_settings(), span_exporter=exporter)
    with client.start_as_current_observation(as_type="span", name="root"):
        with trace_observation(
            "agent.step.tool", input={"tool": "list_feeds"}, metadata={"ok": True}
        ) as obs:
            assert obs is not None
    client.flush()
    spans = {s.name: s for s in exporter.get_finished_spans()}
    assert "agent.step.tool" in spans
    child, root = spans["agent.step.tool"], spans["root"]
    assert child.parent is not None
    assert child.parent.span_id == root.context.span_id, (
        "observation must be explicitly parented to the active langfuse span"
    )


def test_trace_observation_does_not_hijack_the_otel_context():
    """Regression (review C1): the observation must NOT become the ambient
    OTel 'current span' — it used to, which re-parented every Jaeger span
    created inside the wrapped block onto a langfuse-only span id."""
    exporter = InMemorySpanExporter()
    client = setup_langfuse(_settings(), span_exporter=exporter)
    with client.start_as_current_observation(as_type="span", name="root") as root:
        with trace_observation("lf-step") as obs:
            assert obs is not None
            current = trace.get_current_span()
            assert current.get_span_context().span_id == int(root.id, 16), (
                "trace_observation changed the ambient OTel current span"
            )


def test_trace_observation_skips_when_no_langfuse_span_is_active():
    """With tracing configured but no langfuse span innermost (e.g. a code
    path outside the traced run), the helper is a no-op — a detached
    observation would strand a one-span trace."""
    exporter = InMemorySpanExporter()
    setup_langfuse(_settings(), span_exporter=exporter)
    with trace_observation("stray") as obs:
        assert obs is None
    assert exporter.get_finished_spans() == ()


# ---------------------------------------------------------------------------
# Credential scrubbing (spec "Credentials never reach the trace backend")
# ---------------------------------------------------------------------------


def test_scrub_text_redacts_bearer_tokens():
    raw = "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.cGF5bG9hZA.c2lnbmF0dXJl"
    out = scrub_text(raw)
    assert "eyJhbGciOiJIUzI1NiJ9" not in out
    assert "c2lnbmF0dXJl" not in out
    assert "[REDACTED]" in out


def test_scrub_text_redacts_bare_jwt():
    raw = "token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.Zm9vYmFyYmF6"
    out = scrub_text(raw)
    assert "eyJhbGciOiJIUzI1NiJ9" not in out


def test_scrub_text_redacts_api_key_shapes():
    assert "sk-lf-nyx-dev-agent" not in scrub_text("key sk-lf-nyx-dev-agent here")
    assert "sk-proj-abcdefghijklmnop" not in scrub_text("sk-proj-abcdefghijklmnop")


def test_scrub_text_leaves_normal_text_alone():
    raw = "list feeds and summarize the second one; skates are mentioned"
    assert scrub_text(raw) == raw


def test_scrub_text_is_idempotent():
    raw = 'headers={"Authorization": "Bearer abcdefghijklmnopqrstuvwxyz"}'
    once = scrub_text(raw)
    assert scrub_text(once) == once


def test_scrub_value_recurses_containers():
    data = {
        "args": {"header": "Bearer abcdefghijklmnopqr", "feed": "news"},
        "list": ["sk-lf-whatever-key", 7, None],
    }
    out = scrub_value(data)
    assert "abcdefghijklmnopqr" not in str(out)
    assert "sk-lf-whatever-key" not in str(out)
    assert out["args"]["feed"] == "news"
    assert out["list"][1:] == [7, None]


def test_mask_langfuse_data_hook_contract():
    """The SDK calls `mask(data=...)` keyword-only and expects JSON-serializable back."""
    out = mask_langfuse_data(data={"prompt": "Bearer 0123456789abcdef"})
    assert "0123456789abcdef" not in str(out)


def _fake_otel_params(attributes: dict):
    """Build a minimal MaskOtelSpansParams for the export-stage hook.

    Untyped dicts in place of `langfuse.types` dataclasses: the hook only
    reads `.spans.items()`, `.attributes.items()`, and echoes identifiers,
    so duck-typed stand-ins keep the test independent of SDK dataclass
    field churn.
    """

    class _Span:
        def __init__(self, attributes):
            self.attributes = attributes

    class _Params:
        def __init__(self, spans):
            self.spans = spans

    return _Params({"id-1": _Span(attributes)})


def test_mask_otel_spans_patches_only_changed_attributes():
    params = _fake_otel_params(
        {
            "langfuse.observation.input": '{"header": "Bearer abcdefghijklmnopqrst"}',
            "gen_ai.usage.input_tokens": 12,
            "clean.attr": "no credentials here",
        }
    )
    result = mask_langfuse_otel_spans(params=params)
    assert result is not None
    patch = result.span_patches["id-1"]
    assert "abcdefghijklmnopqrst" not in str(patch.set_attributes)
    assert "[REDACTED]" in patch.set_attributes["langfuse.observation.input"]
    assert "clean.attr" not in patch.set_attributes
    assert "gen_ai.usage.input_tokens" not in patch.set_attributes


def test_mask_otel_spans_returns_none_when_batch_is_clean():
    params = _fake_otel_params({"langfuse.observation.output": "plain reply"})
    assert mask_langfuse_otel_spans(params=params) is None


_LOG_FORMAT = "%(asctime)s %(levelname)s %(name)s: %(message)s"
_LOG_FORMAT_RE = re.compile(
    r"\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3} "
    r"(?P<level>[A-Z]+) (?P<name>[^:]+): (?P<message>.*)",
    re.DOTALL,
)


@pytest.fixture
def _restore_logging():
    root = logging.getLogger()
    root_level = root.level
    agent_level = logging.getLogger("agent").level
    yield
    root.setLevel(root_level)
    logging.getLogger("agent").setLevel(agent_level)


def test_configure_logging_installs_one_formatted_root_handler(
    monkeypatch, _restore_logging
):
    """D1: root at WARNING with the timestamp/level/logger format, installed
    once — so a record the process emits renders with all three fields."""
    root = logging.getLogger()
    monkeypatch.setattr(root, "handlers", [])

    configure_logging()

    assert root.level == logging.WARNING
    assert len(root.handlers) == 1
    rendered = root.handlers[0].formatter.format(
        logging.LogRecord(
            "agent.observability",
            logging.WARNING,
            __file__,
            1,
            "export failed for %s",
            ("http://langfuse:3000",),
            None,
        )
    )
    match = _LOG_FORMAT_RE.fullmatch(rendered)
    assert match is not None, rendered
    assert match.group("level") == "WARNING"
    assert match.group("name") == "agent.observability"
    assert match.group("message") == "export failed for http://langfuse:3000"


def test_configure_logging_raises_agent_logger_to_info_and_is_idempotent(
    monkeypatch, _restore_logging
):
    """D1: `agent` at INFO (third-party INFO stays on the root WARNING), and a
    repeated setup leaves the handler set untouched — the second call is a
    no-op rather than a duplicate handler."""
    root = logging.getLogger()
    monkeypatch.setattr(root, "handlers", [])

    configure_logging()

    assert logging.getLogger("agent").level == logging.INFO
    assert logging.getLogger("agent.observability").getEffectiveLevel() == logging.INFO
    first_pass = list(root.handlers)
    assert len(first_pass) == 1

    configure_logging()

    assert root.handlers == first_pass


def test_startup_line_is_dropped_until_the_agent_logger_is_raised(caplog, _restore_logging):
    """The exact bug: with the pre-change configuration (no root handler, no
    `agent` level) the startup announcement never reaches an operator; after
    `configure_logging` it names the host at INFO."""
    logging.getLogger("agent").setLevel(logging.WARNING)

    caplog.clear()
    setup_langfuse(_settings(), span_exporter=InMemorySpanExporter())
    assert [r for r in caplog.records if r.name.startswith("agent.")] == []

    shutdown_langfuse()
    caplog.clear()
    configure_logging()
    setup_langfuse(_settings(), span_exporter=InMemorySpanExporter())

    lines = [r for r in caplog.records if r.name == "agent.observability"]
    assert len(lines) == 1, [(r.levelno, r.name, r.getMessage()) for r in caplog.records]
    assert lines[0].levelno == logging.INFO
    assert "langfuse tracing enabled" in lines[0].getMessage()
    assert "127.0.0.1:9" in lines[0].getMessage()


def test_disabled_tracing_logs_no_langfuse_line(caplog, _restore_logging):
    """Opt-in gating means silence: disabled never constructs the SDK, so not
    even at INFO may a Langfuse line appear."""
    configure_logging()
    caplog.clear()

    assert setup_langfuse(Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")) is None

    assert [r for r in caplog.records if "langfuse" in r.getMessage().lower()] == []
