from __future__ import annotations

import logging

import pytest
from opentelemetry.sdk.trace.export import SpanExporter, SpanExportResult

from agent import observability as obs
from agent.config import Settings


class _RecordingExporter(SpanExporter):
    def export(self, spans):
        return SpanExportResult.SUCCESS

    def shutdown(self):
        return None


@pytest.fixture(autouse=True)
def _isolate_provider(monkeypatch):
    monkeypatch.setattr(obs, "_provider", None)
    yield
    obs.shutdown_observability()


def _settings(**overrides):
    return Settings(_env_file=None, llm_base_url="http://vllm:8000/v1", **overrides)


def test_otel_disabled_constructs_no_exporter(monkeypatch, caplog):
    def _fail(*args, **kwargs):
        raise AssertionError("OTLPSpanExporter constructed while disabled")

    def _fail_set(provider):
        raise AssertionError("global tracer provider claimed while disabled")

    monkeypatch.setattr(obs, "OTLPSpanExporter", _fail)
    monkeypatch.setattr(obs.trace, "set_tracer_provider", _fail_set)

    with caplog.at_level(logging.INFO, logger="agent.observability"):
        obs.setup_observability(_settings(otel_enabled=False))

    assert obs._provider is None
    assert any(
        "Jaeger span export disabled" in record.getMessage()
        for record in caplog.records
    )


def test_otel_enabled_constructs_exporter_with_endpoint(monkeypatch, caplog):
    captured = {}

    def _fake_exporter(**kwargs):
        captured.update(kwargs)
        return _RecordingExporter()

    monkeypatch.setattr(obs, "OTLPSpanExporter", _fake_exporter)
    monkeypatch.setattr(obs.trace, "set_tracer_provider", lambda provider: None)

    with caplog.at_level(logging.INFO, logger="agent.observability"):
        obs.setup_observability(
            _settings(otel_enabled=True, otel_endpoint="http://jaeger:4318")
        )

    assert obs._provider is not None
    assert captured["endpoint"] == "http://jaeger:4318/v1/traces"
    assert captured["session"].trust_env is False
    assert any(
        "Jaeger span export enabled" in record.getMessage()
        for record in caplog.records
    )
