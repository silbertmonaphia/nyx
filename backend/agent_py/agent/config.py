"""Pydantic-settings config for the Python agent service.

`AGENT_BUDGET_PER_USER_PER_HOUR` is intentionally unprefixed — it's a shared env var
per FUTURE_AGENT.md, historical cross-runtime contract. Read it directly via os.getenv.
"""

from __future__ import annotations

import os

from pydantic import model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        env_prefix="PYTHON_AGENT_",
        env_file=".env",
        extra="ignore",
    )

    # --- Server ---
    port: int = 9000
    metrics_port: int = 9001

    # --- LLM ---
    llm_base_url: str = "https://api.openai.com/v1"
    llm_api_key: str = ""
    llm_model: str = "gpt-4o-mini"
    llm_planner_model: str = ""

    # --- Plan / step caps ---
    plan_max_steps: int = 10
    step_react_max_iterations: int = 5

    # --- Backend bridges ---
    # Host-dev defaults: `make dev-agent` runs the agent on the host and
    # `make dev-backend` serves on :8080 there. docker-compose.yml sets the
    # in-network hostnames (`http://backend:8080/...`) explicitly for the
    # agent container. Do NOT default to compose-only hostnames: on the
    # host they don't resolve and the bridge fails (RAG degrades to empty
    # context; tools error).
    mcp_url: str = "http://localhost:8080/mcp"
    rag_bridge_url: str = "http://localhost:8080/internal/rag/retrieve"

    # --- Redis ---
    # Host-dev default. NOTE: the host-mapped port is 6380 (compose maps
    # 6380->6379 to avoid clashing with a native Redis). Defaulting to the
    # compose hostname made every llm step fail with `budget_unavailable`
    # ("Budget guard unavailable") — 2026-10 dev report.
    redis_url: str = "redis://localhost:6380"

    # --- OpenTelemetry ---
    # Host-dev default: `make dev-agent` runs on the host and Jaeger
    # publishes 4318 there — mirrors the Go backend's
    # OTEL_EXPORTER_OTLP_ENDPOINT default (`config.go`). docker-compose.yml
    # overrides this with the in-network hostname (`jaeger:4318`) for the
    # agent container. Do NOT default to the compose hostname: on the host
    # it doesn't resolve, and with HTTP_PROXY set the request goes through
    # the proxy, which returns 502 ("Transient error Bad Gateway
    # encountered while exporting spans batch" — 2026-10 dev-agent report).
    otel_endpoint: str = "http://localhost:4318"
    otel_service_name: str = "nyx-agent"

    # --- Per-IP rate limit (defense in depth; /agent/run is on the
    # private network and has no app-layer auth — see README "Trust
    # model" + docker-compose.yml networks: section). Limits apply
    # per source IP per 1-second sliding window. Generous enough that
    # a single Go backend instance never trips; tight enough that a
    # compromised container can't drain the LLM budget.
    requests_per_second: float = 100.0
    burst: int = 200

    # --- Run-state persistence (Postgres via langGraph PostgresSaver + thin
    # `agent.runs` metadata table — see openspec/changes/agent-run-state/).
    # Required when AGENT_ENABLED=true; alembic migrations run automatically
    # on lifespan startup (same posture as the Go backend's golang-migrate on
    # boot). Compose prod path sets `migrations_disabled=true` and relies on
    # the dedicated `agent-migrate` one-shot service to run migrations before
    # `agent` replicas boot — same single-source-of-truth story as Go.
    database_url: str = ""
    migrations_disabled: bool = False
    retention_enabled: bool = True
    retention_days: int = 30
    retention_interval_seconds: int = 3600
    database_pool_max_size: int = 10
    database_pool_acquire_timeout_seconds: float = 5.0

    # NOTE: there is intentionally NO JWT verification config here. The Python
    # service trusts the network boundary (compose bridge / k8s NetworkPolicy
    # restricting ingress to the Go backend). Go verifies the user JWT at the
    # SPA edge and forwards the verified user_id via the `X-Nyx-User-Id`
    # header; the bearer token is forwarded as a passthrough to the MCP and
    # RAG bridges (Go re-verifies on those endpoints). See FUTURE_AGENT.md +
    # docker-compose.yml for the trust model.

    @model_validator(mode="after")
    def _require_openai_key(self) -> "Settings":
        """Refuse to start when the LLM target is OpenAI proper but no
        API key is configured. Local vLLM / OpenRouter don't require a
        key (vLLM may run with --api-key empty; OpenRouter uses a
        Bearer token but the URL doesn't start with api.openai.com).

        This is the spec scenario "Required key missing refuses startup"
        narrowed to the case where missing the key is genuinely
        catastrophic (every LLM call returns 401 immediately).
        """
        if (
            not self.llm_api_key
            and self.llm_base_url.startswith("https://api.openai.com/")
        ):
            raise ValueError(
                "PYTHON_AGENT_LLM_API_KEY is required when "
                "PYTHON_AGENT_LLM_BASE_URL targets api.openai.com; "
                "set the key or point LLM_BASE_URL at a local vLLM."
            )
        return self

    @model_validator(mode="after")
    def _normalize_database_url(self) -> "Settings":
        """Ensure `PYTHON_AGENT_DATABASE_URL` carries `options=-c search_path=langgraph,agent`.

        Consumers of this DSN:
          - asyncpg pool (`RunMetadata`): all its SQL uses unqualified
            `runs` — this search_path (the `agent` entry) is what makes
            those resolve. The pool is the only consumer that actually
            relies on the DSN options.
          - alembic: carries the DSN but re-SETs its own search_path in
            `migrations/env.py`.
          - langGraph saver (psycopg): `options=` is stripped before the
            URL reaches psycopg (`_build_checkpointer`) — psycopg passes
            URL options as `-c name=value` CLI args to postgres, which
            chokes on `=`. The saver's search_path is set explicitly in
            `main.py` instead.

        Canonical order is `langgraph,agent` — langgraph FIRST so that if
        any future code path ever resolves unqualified langGraph DDL
        through this search_path, `checkpoint_*` tables still land in
        `langgraph.*` rather than `agent.*`. Any operator-supplied
        `options=` (encoded or not) is stripped and replaced by this
        canonical value. Excludes `public` so the Python agent is
        structurally unable to touch Go's tables — a typo like `FROM
        feed` raises `relation does not exist` instead of silently
        corrupting Go's data.

        Note: the value is URL-encoded — psycopg's URL parser rejects
        `=` inside a query value; asyncpg accepts both forms.
        """
        from urllib.parse import quote

        url = self.database_url
        if not url:
            return self

        # Strip any pre-existing `options=...` key/value from the query
        # string (both encoded and unencoded forms). Otherwise operators
        # who paste `options=-c search_path=...` literally end up with
        # TWO options= keys after the validator appends the encoded one.
        # Match the unencoded form (bare `=`) and the encoded forms
        # (`%3D` for `=`, `%26` for `&`).
        import re as _re

        url = _re.sub(r"&?options=[^&]*", "", url)
        # Strip a leading `?` if we just removed the only query param.
        url = _re.sub(r"\?$", "", url)

        sep = "&" if "?" in url else "?"
        encoded_value = quote("-c search_path=langgraph,agent", safe="")
        url = f"{url}{sep}options={encoded_value}"
        object.__setattr__(self, "database_url", url)
        return self

    @property
    def planner_model(self) -> str:
        return self.llm_planner_model or self.llm_model

    @property
    def budget_per_user_per_hour(self) -> int:
        """Per-user hourly token cap. Read from the unprefixed `AGENT_BUDGET_PER_USER_PER_HOUR`
        env var (shared with Go historically per FUTURE_AGENT.md §P1.2). Defaults to 200000
        when unset.
        """
        raw = os.getenv("AGENT_BUDGET_PER_USER_PER_HOUR", "200000")
        try:
            return int(raw)
        except ValueError:
            return 200000


_settings: Settings | None = None


def get_settings() -> Settings:
    """Process-wide settings singleton (built lazily)."""
    global _settings
    if _settings is None:
        _settings = Settings()
    return _settings