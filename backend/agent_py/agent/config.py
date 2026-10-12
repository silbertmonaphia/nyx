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
    mcp_url: str = "http://backend:8080/mcp"
    rag_bridge_url: str = "http://backend:8080/internal/rag/retrieve"

    # --- Redis ---
    redis_url: str = "redis://redis:6379"

    # --- OpenTelemetry ---
    otel_endpoint: str = "http://jaeger:4318"
    otel_service_name: str = "nyx-agent"

    # --- Per-IP rate limit (defense in depth; /agent/run is on the
    # private network and has no app-layer auth — see README "Trust
    # model" + docker-compose.yml networks: section). Limits apply
    # per source IP per 1-second sliding window. Generous enough that
    # a single Go backend instance never trips; tight enough that a
    # compromised container can't drain the LLM budget.
    requests_per_second: float = 100.0
    burst: int = 200

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