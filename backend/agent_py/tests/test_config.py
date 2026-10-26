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
        ("langfuse_host", "http://localhost:3000"),
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


def test_sweep_settings_defaults(monkeypatch):
    """Spec scenario "Default values applied when env unset": heartbeat 30s,
    stale timeout 900s, sweep every 300s, sweeper on
    (agent-stale-run-sweeper)."""
    for key in (
        "HEARTBEAT_INTERVAL_SECONDS",
        "STALE_RUN_TIMEOUT_SECONDS",
        "SWEEP_INTERVAL_SECONDS",
        "SWEEP_ENABLED",
    ):
        monkeypatch.delenv(f"PYTHON_AGENT_{key}", raising=False)
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.heartbeat_interval_seconds == 30
    assert s.stale_run_timeout_seconds == 900
    assert s.sweep_interval_seconds == 300
    assert s.sweep_enabled is True


def test_misconfigured_sweep_ratio_refused():
    """Spec scenario "Misconfigured sweep ratio refuses startup": a stale
    timeout below 2x the heartbeat interval would let the sweeper reap
    live runs — refuse at startup naming both vars."""
    with pytest.raises(ValueError) as excinfo:
        Settings(
            _env_file=None,
            llm_base_url="http://vllm:8000/v1",
            heartbeat_interval_seconds=30,
            stale_run_timeout_seconds=45,
        )
    msg = str(excinfo.value)
    assert "PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS" in msg
    assert "PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS" in msg


@pytest.mark.parametrize(
    "kwargs",
    [
        {"heartbeat_interval_seconds": 0},
        {"heartbeat_interval_seconds": -1},
        {"sweep_interval_seconds": 0},
        {"sweep_interval_seconds": -1},
    ],
)
def test_below_one_sweep_intervals_refused(kwargs):
    """Heartbeat / sweep intervals below 1 are operator mistakes —
    refuse at startup naming the offending env var."""
    with pytest.raises(ValueError) as excinfo:
        Settings(_env_file=None, llm_base_url="http://vllm:8000/v1", **kwargs)
    key = next(iter(kwargs)).upper()
    assert f"PYTHON_AGENT_{key}" in str(excinfo.value)


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


def test_langfuse_defaults(monkeypatch):
    """Spec scenario "Langfuse defaults applied when unset": tracing is
    off, the host default is host-reachable, and no credentials are
    required while disabled (agent-langfuse-tracing)."""
    for key in (
        "LANGFUSE_ENABLED",
        "LANGFUSE_HOST",
        "LANGFUSE_PUBLIC_KEY",
        "LANGFUSE_SECRET_KEY",
    ):
        monkeypatch.delenv(f"PYTHON_AGENT_{key}", raising=False)
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.langfuse_enabled is False
    assert s.langfuse_host == "http://localhost:3000"
    assert s.langfuse_public_key == ""
    assert s.langfuse_secret_key.get_secret_value() == ""


def test_otel_defaults(monkeypatch):
    for key in ("OTEL_ENABLED", "OTEL_ENDPOINT", "OTEL_SERVICE_NAME"):
        monkeypatch.delenv(f"PYTHON_AGENT_{key}", raising=False)
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.otel_enabled is True
    assert s.otel_endpoint == "http://localhost:4318"
    assert s.otel_service_name == "nyx-agent"


def test_otel_enabled_env_override(monkeypatch):
    monkeypatch.setenv("PYTHON_AGENT_OTEL_ENABLED", "false")
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.otel_enabled is False


@pytest.mark.parametrize(
    "kwargs,missing_var",
    [
        pytest.param(
            {"langfuse_public_key": "", "langfuse_secret_key": "sk-lf-x"},
            "PYTHON_AGENT_LANGFUSE_PUBLIC_KEY",
            id="public-key-missing",
        ),
        pytest.param(
            {"langfuse_public_key": "pk-lf-x", "langfuse_secret_key": ""},
            "PYTHON_AGENT_LANGFUSE_SECRET_KEY",
            id="secret-key-missing",
        ),
        pytest.param(
            {"langfuse_public_key": "", "langfuse_secret_key": ""},
            "PYTHON_AGENT_LANGFUSE_PUBLIC_KEY",
            id="both-missing",
        ),
    ],
)
def test_langfuse_enabled_without_credentials_refused(kwargs, missing_var):
    """Spec scenario "Langfuse enabled without credentials refuses
    startup": enabling tracing with an empty key pair is an operator
    mistake — refuse at boot naming the missing env var."""
    with pytest.raises(ValueError) as excinfo:
        Settings(
            _env_file=None,
            llm_base_url="http://vllm:8000/v1",
            langfuse_enabled=True,
            **kwargs,
        )
    assert missing_var in str(excinfo.value)


def test_langfuse_enabled_with_credentials_accepted():
    """A full key pair (the documented bring-up path) boots cleanly."""
    s = Settings(
        _env_file=None,
        llm_base_url="http://vllm:8000/v1",
        langfuse_enabled=True,
        langfuse_public_key="pk-lf-nyx-dev-agent",
        langfuse_secret_key="sk-lf-nyx-dev-agent",
    )
    assert s.langfuse_enabled is True
    # SecretStr must not leak the raw value via repr/logging.
    assert "sk-lf-nyx-dev-agent" not in repr(s)


# --- web tools (openspec/changes/agent-web-tools) -------------------------

_WEB_ENV_KEYS = (
    "WEB_FETCH_ENABLED",
    "WEB_SEARCH_ENABLED",
    "TAVILY_API_KEY",
    "TAVILY_BASE_URL",
    "WEB_SEARCH_MAX_RESULTS",
    "WEB_FETCH_MAX_BYTES",
    "WEB_FETCH_TIMEOUT_SECONDS",
    "WEB_FETCH_MAX_CHARS",
    "WEB_FETCH_MAX_REDIRECTS",
    "WEB_FETCH_ALLOW_PRIVATE",
)


def test_web_tools_default_off(monkeypatch):
    """Spec scenario "Defaults disable both tools": with nothing set, the
    service starts and no web tool is registered — the merged tool list
    must equal the MCP registry list (agent-web-tools delta)."""
    for key in _WEB_ENV_KEYS:
        monkeypatch.delenv(f"PYTHON_AGENT_{key}", raising=False)
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.web_fetch_enabled is False
    assert s.web_search_enabled is False
    assert s.web_search_max_results == 5
    assert s.web_fetch_max_bytes == 2 * 1024 * 1024
    assert s.web_fetch_timeout_seconds == 10
    assert s.web_fetch_max_chars == 20000
    assert s.web_fetch_max_redirects == 5
    assert s.web_fetch_allow_private is False
    assert s.tavily_base_url == "https://api.tavily.com/search"
    # SecretStr must not leak the raw value via repr/logging.
    assert s.tavily_api_key.get_secret_value() == ""


def test_web_search_enabled_keyless_boots(monkeypatch):
    """Spec scenario "Keyless search boots without a key": Tavily's
    keyless tier is a supported mode — enabling search without a key must
    start cleanly (the provider-swap replaced the old refuse-start rule)."""
    for key in _WEB_ENV_KEYS:
        monkeypatch.delenv(f"PYTHON_AGENT_{key}", raising=False)
    s = Settings(
        _env_file=None,
        llm_base_url="http://vllm:8000/v1",
        web_search_enabled=True,
    )
    assert s.web_search_enabled is True
    assert s.tavily_api_key.get_secret_value() == ""


def test_web_search_enabled_with_key_accepted(monkeypatch):
    for key in _WEB_ENV_KEYS:
        monkeypatch.delenv(f"PYTHON_AGENT_{key}", raising=False)
    s = Settings(
        _env_file=None,
        llm_base_url="http://vllm:8000/v1",
        web_search_enabled=True,
        tavily_api_key="tvly-test-key",
    )
    assert s.web_search_enabled is True
    assert "tvly-test-key" not in repr(s)


@pytest.mark.parametrize(
    "field",
    [
        "web_search_max_results",
        "web_fetch_max_bytes",
        "web_fetch_timeout_seconds",
        "web_fetch_max_chars",
        "web_fetch_max_redirects",
    ],
)
def test_web_tools_negative_numeric_refused(monkeypatch, field):
    """Spec scenario "Negative numeric setting refuses startup"."""
    for key in _WEB_ENV_KEYS:
        monkeypatch.delenv(f"PYTHON_AGENT_{key}", raising=False)
    with pytest.raises(ValueError) as excinfo:
        Settings(
            _env_file=None,
            llm_base_url="http://vllm:8000/v1",
            **{field: -1},
        )
    assert f"PYTHON_AGENT_{field.upper()}" in str(excinfo.value)


# --- chat sessions (openspec/changes/agent-chat-history-ui) ---------------


def test_chat_sessions_defaults(monkeypatch):
    """Spec scenario "Default values applied when env unset": page size 10;
    both bounds of the page-size range start."""
    monkeypatch.delenv("PYTHON_AGENT_SESSIONS_PAGE_SIZE", raising=False)
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.sessions_page_size == 10
    # The range endpoints are valid configuration, not off-by-one bugs.
    assert (
        Settings(
            _env_file=None, llm_base_url="http://vllm:8000/v1", sessions_page_size=1
        ).sessions_page_size
        == 1
    )
    assert (
        Settings(
            _env_file=None, llm_base_url="http://vllm:8000/v1", sessions_page_size=50
        ).sessions_page_size
        == 50
    )


@pytest.mark.parametrize("value", [0, 51, -1])
def test_sessions_page_size_out_of_range_refused(value):
    """Spec scenario "Page size out of range refuses startup"."""
    with pytest.raises(ValueError) as excinfo:
        Settings(
            _env_file=None,
            llm_base_url="http://vllm:8000/v1",
            sessions_page_size=value,
        )
    assert "PYTHON_AGENT_SESSIONS_PAGE_SIZE must be in [1, 50]" in str(excinfo.value)


def test_approval_ttl_defaults_to_300():
    """Spec scenario "Default TTL applied when unset": a pending
    confirmation expires 300 s after the `confirm_request` frame
    (agent-feed-tools)."""
    s = Settings(_env_file=None, llm_base_url="http://vllm:8000/v1")
    assert s.approval_ttl_seconds == 300


@pytest.mark.parametrize("value", [0, -1])
def test_approval_ttl_below_one_refused(value):
    """Spec scenario "Invalid TTL refuses startup"."""
    with pytest.raises(ValueError) as excinfo:
        Settings(
            _env_file=None,
            llm_base_url="http://vllm:8000/v1",
            approval_ttl_seconds=value,
        )
    assert "PYTHON_AGENT_APPROVAL_TTL_SECONDS must be >= 1" in str(excinfo.value)
