"""Pydantic-settings config for the Python agent service.

`AGENT_BUDGET_PER_USER_PER_HOUR` is intentionally unprefixed — it's a shared env var
per FUTURE_AGENT.md, historical cross-runtime contract. Read it directly via os.getenv.
"""

from __future__ import annotations

import os

from pydantic import SecretStr, model_validator
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

    # --- Planner robustness (openspec/changes/agent-planner-structured) ---
    # Maximum retries on a Pydantic validation failure from the structured
    # planner binding. 0 = no retry (single attempt). Negative values are
    # rejected at startup. Default 1: one re-prompt with the validator's
    # error message appended under a `PREVIOUS_ATTEMPT_FAILED` block before
    # terminating with `error{code:"plan_invalid"}`.
    planner_retry_max: int = 1
    # Whether the planner obtains `Plan` via provider structured output on
    # the primary path: an explicit `response_format` json_schema bind
    # (`strict: false`) with the content JSON validated through Pydantic
    # (see `planner._build_structured_client`). When false, the legacy
    # free-text path (`json.loads` + fence-stripping) is used. The flag
    # exists so vLLM deployments on a parser that doesn't honour
    # `response_format` can opt out without code change; the gate is
    # symmetric to `llm_fallback_*`.
    planner_structured_enabled: bool = True

    # --- Tool-dispatch robustness (openspec/changes/agent-tool-dispatch-retry) ---
    # Maximum retries on a tool-dispatch failure from the executor.
    # Covers two failure classes: registry-rejected tool calls
    # (Pydantic validation, missing required field, wrong type) and
    # transient errors from the tool's remote call (network, MCP /
    # RAG bridge timeouts). 0 = no retry (single attempt). Negative
    # values are rejected at startup. The budget is per-step and
    # per-tool — a retry exhausted on one tool does not consume budget
    # for sibling tools in the same step, and a retry exhausted on one
    # step does not consume budget for later steps.
    tool_retry_max: int = 2
    # Base backoff between retries on transient tool-execution errors.
    # Actual sleep is `min(base * 2^attempt + jitter, ceiling)` with
    # jitter ±20%. Only the transient path uses backoff; the
    # validator-feedback path re-prompts the model immediately (no
    # sleep). Integer milliseconds to keep the math round and avoid
    # floating-point drift across the retry loop.
    tool_retry_backoff_base_ms: int = 250
    # Hard ceiling on the per-attempt backoff sleep. Cumulative
    # wall-clock across the step's transient retries is bounded by
    # `tool_retry_max * ceiling`; the step's overall timeout is
    # enforced by the existing `AGENT_STEP_TIMEOUT` semantics, not by
    # the retry loop.
    tool_retry_backoff_ceiling_ms: int = 5000

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

    # --- Chat-history retention (openspec/changes/agent-chat-history-storage) ---
    # Retention floor for `agent.chat_sessions` and `agent.chat_messages`.
    # The user requirement is ≥1 year (one year is the minimum; longer
    # retention is an operator choice). Refuses to start below 365 — the
    # user-facing floor is non-negotiable.
    chat_retention_days: int = 365
    # Soft-delete grace period before hard-delete. A row whose
    # `last_active_at` is past `chat_retention_days` is soft-deleted on
    # the first retention iteration that observes it, then hard-deleted
    # after `chat_retention_grace_days` has elapsed. 0 = single-phase
    # hard-delete. Slice 4 (GDPR) is the only consumer that reads the
    # soft-deleted state.
    chat_retention_grace_days: int = 7
    # Kill switch for the chat retention cron. When false, the cron is
    # not constructed and no chat rows are deleted; writes continue (so a
    # future flip-back resumes normal retention). Rollback path.
    chat_retention_enabled: bool = True

    # --- Chat-session API (openspec/changes/agent-chat-history-ui) ---
    # Default page size for `GET /agent/sessions`. The request's `limit`
    # query param overrides it per call; requests are hard-capped at 50
    # (same bound the validator below enforces on this default).
    sessions_page_size: int = 10

    # --- User preferences (openspec/changes/agent-user-preferences) ---
    # Kill switch for the whole preference memory layer: when false, no
    # extraction task fires after a turn AND the planner prompt omits the
    # injected block — stored rows stay in `agent.chat_user_prefs`
    # untouched. Rollback path (design.md D7).
    prefs_enabled: bool = True
    # Cap on preferences injected into the planner system prompt: the
    # planner loads the top-N by `updated_at DESC` per user. 0 = rows may
    # still be extracted/upserted but nothing is injected. Negative values
    # are rejected at startup (validator below). 50 is generous for the
    # flat key/value regime (design.md risk "Prompt growth").
    prefs_max_injected: int = 50

    # --- GDPR erasure + export (openspec/changes/agent-gdpr-compliance) ---
    # Kill switch for `GET /agent/me/export` and `DELETE /agent/me`. When
    # false, both respond 503 without reading data and without writing an
    # audit row ("the forbidden operation never happened"). Default true:
    # erasure is legally load-bearing, so the safe posture is on-by-default
    # — a disabled-by-default compliance endpoint is a latent incident
    # (design.md D6). The flag exists for the rollout drill and for an
    # operator who must temporarily freeze destructive endpoints.
    gdpr_enabled: bool = True
    # Retention window for `agent.data_audit_log`, in days. Default 1095
    # (3 years) deliberately outlives the 1-year chat retention so the
    # erase/export audit trail remains verifiable after the underlying
    # data would have been reaped anyway (design.md D5). MUST be >= 365 —
    # the audit trail must always outlive the data it audits; refuses to
    # start below (validator below).
    audit_retention_days: int = 1095

    # --- Mutating-call approval TTL (openspec/changes/agent-feed-tools) ---
    # Wall-clock window a paused mutating-call confirmation stays open
    # before it closes as denied (design.md D6). MUST be >= 1 — a zero or
    # negative window would deny every mutating call instantly (validator
    # below); the default matches the design's 300 s.
    approval_ttl_seconds: int = 300

    # --- Stale-run sweeper (openspec/changes/agent-stale-run-sweeper) ---
    # Heartbeat cadence for live runs: while a run's SSE stream is in
    # flight, a background task touches `agent.runs.last_heartbeat_at`
    # this often. Touch failures are non-fatal (log-and-continue).
    heartbeat_interval_seconds: int = 30
    # A `running` row whose heartbeat is older than this is considered
    # dead and reaped to `status='failed', error_code='stale_run'`. The
    # threshold doubles as the window in which a crashed run can still
    # be resumed by echoing its run_id; after reaping, resume follows
    # the terminal-row path (fresh run). MUST be >= 2x
    # heartbeat_interval_seconds (validated below) so a transient
    # heartbeat hiccup cannot get a live run reaped.
    stale_run_timeout_seconds: int = 900
    # Sweeper cron cadence (mirrors `retention_interval_seconds`).
    sweep_interval_seconds: int = 300
    # Kill switch / rollback path: when false, no rows are reaped.
    # Heartbeats keep writing (inert liveness metadata).
    sweep_enabled: bool = True

    # --- Langfuse LLM tracing (openspec/changes/agent-langfuse-tracing) ---
    # Opt-in export of LLM-level traces (prompts, replies, usage, tool
    # calls) to a self-hosted Langfuse instance. Default OFF: when
    # false, no client is constructed and no payload leaves the process.
    # Refuses to start when enabled without credentials (validator below).
    langfuse_enabled: bool = False
    # Host-dev default: `make up-langfuse` publishes the UI/API on
    # 127.0.0.1:3000 there — same reasoning as mcp_url/otel_endpoint
    # above (compose-only hostnames don't resolve on the host).
    # docker-compose.yml overrides this with the in-network hostname
    # (`http://langfuse-web:3000`) for the agent container.
    langfuse_host: str = "http://localhost:3000"
    langfuse_public_key: str = ""
    langfuse_secret_key: SecretStr = SecretStr("")

    # --- Web tools (openspec/changes/agent-web-tools) ---
    # Opt-in agent-native tools: web_fetch (SSRF-guarded page retrieval)
    # and web_search (Tavily Search API). Both default OFF: enabling
    # introduces outbound egress. When off, the merged tool list equals
    # the MCP registry list and the planner prompt is byte-identical to
    # its pre-web-tools form.
    web_fetch_enabled: bool = False
    web_search_enabled: bool = False
    # Tavily Search API (web_search). Optional: with an empty key the
    # tool uses Tavily's keyless tier (rate-limited, no account needed);
    # with a free key (1k searches/month, no credit card) usage counts
    # against the account. The base URL is the full endpoint so
    # tests/e2e can point it at a local stub.
    tavily_api_key: SecretStr = SecretStr("")
    tavily_base_url: str = "https://api.tavily.com/search"
    # Result caps / bounds. web_search_max_results is clamped to the hard
    # cap of 10 at dispatch time; negative values are rejected at startup.
    web_search_max_results: int = 5
    web_fetch_max_bytes: int = 2 * 1024 * 1024
    web_fetch_timeout_seconds: int = 10
    web_fetch_max_chars: int = 20000
    web_fetch_max_redirects: int = 5
    # Dev/e2e escape hatch: lifts the private-address deny-list AND the
    # 80/443 port restriction so fetches can reach local fixture servers
    # (same posture as the Go backend's LLM_ALLOW_PRIVATE_URL). Logged
    # loudly at startup. NEVER enable in a deployed environment.
    web_fetch_allow_private: bool = False

    # NOTE: there is intentionally NO JWT verification config here. The Python
    # service trusts the network boundary (compose bridge / k8s NetworkPolicy
    # restricting ingress to the Go backend). Go verifies the user JWT at the
    # SPA edge and forwards the verified user_id via the `X-Nyx-User-Id`
    # header; the bearer token is forwarded as a passthrough to the MCP and
    # RAG bridges (Go re-verifies on those endpoints). See FUTURE_AGENT.md +
    # docker-compose.yml for the trust model.

    @model_validator(mode="after")
    def _validate_planner_retry(self) -> Settings:
        """Refuse to start when the planner retry cap is negative.

        Per `openspec/changes/agent-planner-structured` (Configurable via
        environment delta): `PYTHON_AGENT_PLANNER_RETRY_MAX` must be ≥ 0.
        0 means one attempt with no retry (the retry loop's outer bound is
        `planner_retry_max`); any negative value is an operator mistake
        and breaks the closed `[0, retry_max]` semantics.
        """
        if self.planner_retry_max < 0:
            raise ValueError(
                "PYTHON_AGENT_PLANNER_RETRY_MAX must be >= 0; "
                f"got {self.planner_retry_max}"
            )
        return self

    @model_validator(mode="after")
    def _validate_tool_retry(self) -> Settings:
        """Refuse to start when the tool-dispatch retry cap is negative.

        Per `openspec/changes/agent-tool-dispatch-retry` (Configurable via
        environment delta): `PYTHON_AGENT_TOOL_RETRY_MAX` must be ≥ 0.
        0 means one attempt with no retry (the retry loop's outer bound is
        `tool_retry_max`); any negative value is an operator mistake
        and breaks the closed `[0, retry_max]` semantics.
        """
        if self.tool_retry_max < 0:
            raise ValueError(
                "PYTHON_AGENT_TOOL_RETRY_MAX must be >= 0; "
                f"got {self.tool_retry_max}"
            )
        return self

    @model_validator(mode="after")
    def _require_openai_key(self) -> Settings:
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
    def _normalize_database_url(self) -> Settings:
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

    @model_validator(mode="after")
    def _validate_sweep_settings(self) -> Settings:
        """Refuse to start on a heartbeat / sweeper misconfiguration.

        Per `openspec/changes/agent-stale-run-sweeper` (Configurable via
        environment delta): heartbeat and sweep intervals must be >= 1,
        and the stale timeout must be >= 2x the heartbeat interval — a
        smaller ratio would let the sweeper reap live runs whose
        heartbeat task is momentarily behind.
        """
        if self.heartbeat_interval_seconds < 1:
            raise ValueError(
                "PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS must be >= 1; "
                f"got {self.heartbeat_interval_seconds}"
            )
        if self.sweep_interval_seconds < 1:
            raise ValueError(
                "PYTHON_AGENT_SWEEP_INTERVAL_SECONDS must be >= 1; "
                f"got {self.sweep_interval_seconds}"
            )
        if self.stale_run_timeout_seconds < 2 * self.heartbeat_interval_seconds:
            raise ValueError(
                "PYTHON_AGENT_STALE_RUN_TIMEOUT_SECONDS must be >= 2x "
                "PYTHON_AGENT_HEARTBEAT_INTERVAL_SECONDS "
                f"({self.heartbeat_interval_seconds}s); got "
                f"{self.stale_run_timeout_seconds}s"
            )
        return self

    @model_validator(mode="after")
    def _require_langfuse_credentials(self) -> Settings:
        """Refuse to start when tracing is enabled without credentials.

        Per `openspec/changes/agent-langfuse-tracing` (Configurable via
        environment delta): `PYTHON_AGENT_LANGFUSE_ENABLED=true` with an
        empty public or secret key is an operator mistake — the client
        would authenticate-fail on every export, silently dropping all
        traces. Fail fast at startup instead (same posture as the
        planner/tool retry caps). `langfuse_host` needs no check: it has
        a working default for both host and compose topologies.
        """
        if not self.langfuse_enabled:
            return self
        missing = []
        if not self.langfuse_public_key:
            missing.append("PYTHON_AGENT_LANGFUSE_PUBLIC_KEY")
        if not self.langfuse_secret_key.get_secret_value():
            missing.append("PYTHON_AGENT_LANGFUSE_SECRET_KEY")
        if missing:
            raise ValueError(
                " and ".join(missing)
                + " required when PYTHON_AGENT_LANGFUSE_ENABLED=true"
            )
        return self

    @model_validator(mode="after")
    def _validate_chat_retention(self) -> Settings:
        """Refuse to start on a chat-retention misconfiguration.

        Per `openspec/changes/agent-chat-history-storage` (Configurable via
        environment delta): `PYTHON_AGENT_CHAT_RETENTION_DAYS` MUST be ≥
        365 — the one-year floor is a user-facing requirement, not just a
        default. `PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS` MUST be ≥ 0
        (negative values are operator mistakes; 0 means single-phase
        hard-delete).
        """
        if self.chat_retention_days < 365:
            raise ValueError(
                "PYTHON_AGENT_CHAT_RETENTION_DAYS must be >= 365; "
                f"got {self.chat_retention_days}"
            )
        if self.chat_retention_grace_days < 0:
            raise ValueError(
                "PYTHON_AGENT_CHAT_RETENTION_GRACE_DAYS must be >= 0; "
                f"got {self.chat_retention_grace_days}"
            )
        return self

    @model_validator(mode="after")
    def _validate_chat_sessions(self) -> Settings:
        """Refuse to start on a chat-session API misconfiguration.

        Per `openspec/changes/agent-chat-history-ui` (agent-python-service
        delta): `PYTHON_AGENT_SESSIONS_PAGE_SIZE` MUST be in `[1, 50]` —
        it is the default page size AND the ceiling the `limit` query
        param is validated against, so an out-of-range default could
        never be served.
        """
        if not 1 <= self.sessions_page_size <= 50:
            raise ValueError(
                "PYTHON_AGENT_SESSIONS_PAGE_SIZE must be in [1, 50]; "
                f"got {self.sessions_page_size}"
            )
        return self

    @model_validator(mode="after")
    def _validate_prefs(self) -> Settings:
        """Refuse to start on a preference-injection misconfiguration.

        Per `openspec/changes/agent-user-preferences` (agent-python-service
        delta): `PYTHON_AGENT_PREFS_MAX_INJECTED` MUST be >= 0 — the value
        is the `LIMIT` bound of the planner's injection read, so a negative
        value is an operator mistake. 0 disables injection while extraction
        keeps writing rows.
        """
        if self.prefs_max_injected < 0:
            raise ValueError(
                "PYTHON_AGENT_PREFS_MAX_INJECTED must be >= 0; "
                f"got {self.prefs_max_injected}"
            )
        return self

    @model_validator(mode="after")
    def _validate_gdpr(self) -> Settings:
        """Refuse to start on a below-floor audit retention.

        Per `openspec/changes/agent-gdpr-compliance` (agent-python-service
        delta): `PYTHON_AGENT_AUDIT_RETENTION_DAYS` MUST be ≥ 365 so the
        audit trail always outlives the 1-year chat retention it audits
        (design.md D5). `gdpr_enabled` needs no validation — both booleans
        are valid configuration.
        """
        if self.audit_retention_days < 365:
            raise ValueError(
                "PYTHON_AGENT_AUDIT_RETENTION_DAYS must be >= 365; "
                f"got {self.audit_retention_days}"
            )
        return self

    @model_validator(mode="after")
    def _validate_approval_ttl(self) -> Settings:
        """Refuse to start on a non-positive approval TTL.

        Per `openspec/changes/agent-feed-tools` (agent-python-service delta
        "New env key for approval TTL"): the value is the wall-clock window
        a mutating-call confirmation stays open, so 0 or negative would
        deny every mutating call instantly — an operator mistake.
        """
        if self.approval_ttl_seconds < 1:
            raise ValueError(
                "PYTHON_AGENT_APPROVAL_TTL_SECONDS must be >= 1; "
                f"got {self.approval_ttl_seconds}"
            )
        return self

    @model_validator(mode="after")
    def _validate_web_tools(self) -> Settings:
        """Refuse to start on a broken web-tools config.

        Per `openspec/changes/agent-web-tools` (agent-python-service delta
        "New env keys for web tools"): negative numerics break the caps'
        semantics (a negative max_bytes would refuse every body), so they
        are operator mistakes. No key requirement: Tavily's keyless tier
        is a supported mode (search works without an account), so an
        empty `PYTHON_AGENT_TAVILY_API_KEY` is valid configuration.
        """
        for name in (
            "web_search_max_results",
            "web_fetch_max_bytes",
            "web_fetch_timeout_seconds",
            "web_fetch_max_chars",
            "web_fetch_max_redirects",
        ):
            value = getattr(self, name)
            if value < 0:
                raise ValueError(
                    f"PYTHON_AGENT_{name.upper()} must be >= 0; got {value}"
                )
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