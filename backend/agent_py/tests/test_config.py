"""Tests for Settings validation — the OpenAI-key guard that refuses
to start when targeting api.openai.com without a key (spec scenario
"Required key missing refuses startup"), plus dev-loop default guards."""

from __future__ import annotations

import pytest

from agent.config import Settings


@pytest.mark.parametrize(
    "field,expected",
    [
        ("otel_endpoint", "http://localhost:4318"),
        ("mcp_url", "http://localhost:8080/mcp"),
        ("rag_bridge_url", "http://localhost:8080/internal/rag/retrieve"),
        ("redis_url", "redis://localhost:6380"),
    ],
)
def test_host_facing_defaults_are_host_reachable(monkeypatch, field, expected):
    """`make dev-agent` runs the agent on the HOST, so every default that
    points at another service must target the host. The compose-only
    hostnames (`jaeger`, `backend`, `redis`) are set explicitly by
    docker-compose.yml for the agent container.

    Regression guard for a bug class that bit twice (2026-10):
      - `otel_endpoint` default `http://jaeger:4318` → span export went
        through the operator's HTTP proxy → 502 "Transient error Bad
        Gateway..." retry loop on every dev run.
      - `redis_url` default `redis://redis:6379` → every llm step failed
        closed with `budget_unavailable` ("Budget guard unavailable").
    Note the host-mapped Redis port is 6380, not 6379.
    """
    monkeypatch.delenv(f"PYTHON_AGENT_{field.upper()}", raising=False)
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert getattr(s, field) == expected, (
        "default must be host-reachable (see docstring); "
        "compose overrides it for the agent container"
    )


def test_planner_robustness_defaults(monkeypatch):
    """Spec scenario "Default values applied when env unset": the structured
    planner is on with a single retry budgeted (agent-planner-structured)."""
    for key in ("PLANNER_RETRY_MAX", "PLANNER_STRUCTURED_ENABLED"):
        monkeypatch.delenv(f"PYTHON_AGENT_{key}", raising=False)
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.planner_retry_max == 1
    assert s.planner_structured_enabled is True


def test_negative_planner_retry_max_refused():
    """Spec scenario "Negative planner-retry max refuses startup"."""
    with pytest.raises(ValueError) as excinfo:
        Settings(
            _env_file=None,
            llm_base_url="http://vllm:8000/v1",
            planner_retry_max=-1,
        )
    assert "PYTHON_AGENT_PLANNER_RETRY_MAX" in str(excinfo.value)


def test_tool_retry_defaults(monkeypatch):
    """Spec scenario "Default values applied when env unset": the tool-dispatch
    retry budget is 2 with a 250ms backoff base (agent-tool-dispatch-retry)."""
    for key in (
        "TOOL_RETRY_MAX",
        "TOOL_RETRY_BACKOFF_BASE_MS",
        "TOOL_RETRY_BACKOFF_CEILING_MS",
    ):
        monkeypatch.delenv(f"PYTHON_AGENT_{key}", raising=False)
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.tool_retry_max == 2
    assert s.tool_retry_backoff_base_ms == 250
    assert s.tool_retry_backoff_ceiling_ms == 5000


def test_negative_tool_retry_max_refused():
    """Spec scenario "Negative tool-retry max refuses startup"."""
    with pytest.raises(ValueError) as excinfo:
        Settings(
            _env_file=None,
            llm_base_url="http://vllm:8000/v1",
            tool_retry_max=-1,
        )
    assert "PYTHON_AGENT_TOOL_RETRY_MAX" in str(excinfo.value)


def test_settings_accepts_local_vllm_without_key():
    """Local vLLM (and OpenRouter) don't need an API key — empty key
    is fine when the base URL doesn't target api.openai.com."""
    s = Settings(
        llm_base_url="http://vllm:8000/v1",
        llm_api_key="",
    )
    assert s.llm_api_key == ""


def test_settings_accepts_openai_with_key():
    s = Settings(
        llm_base_url="https://api.openai.com/v1",
        llm_api_key="sk-test",
    )
    assert s.llm_api_key == "sk-test"


def test_settings_rejects_openai_without_key():
    """Targeting api.openai.com with an empty key is a configuration
    that would 401 on every request — refuse to boot."""
    with pytest.raises(ValueError) as excinfo:
        Settings(
            llm_base_url="https://api.openai.com/v1",
            llm_api_key="",
        )
    assert "PYTHON_AGENT_LLM_API_KEY" in str(excinfo.value)
    assert "api.openai.com" in str(excinfo.value)


def test_settings_accepts_openai_subdomain_with_key():
    """Only the canonical api.openai.com host triggers the guard;
    a custom proxy or mirror with a similar URL is fine as long as
    the operator provides a key."""
    s = Settings(
        llm_base_url="https://api.openai.com.example.com/v1",
        llm_api_key="anything",
    )
    assert s.llm_api_key == "anything"
