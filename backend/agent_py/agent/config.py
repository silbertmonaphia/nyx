"""Pydantic-settings config for the Python agent service.

`AGENT_BUDGET_PER_USER_PER_HOUR` is intentionally unprefixed — it's a shared env var
per FUTURE_AGENT.md, historical cross-runtime contract. Read it directly via os.getenv.
"""

from __future__ import annotations

import os

from pydantic import field_validator
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

    # --- JWT verification (CRITICAL-3 follow-up + S-1 fix) ---
    # `jwt_secret` is the shared HMAC key with Go's auth package. When set, the
    # Python service verifies the inbound JWT signature (HS256 by default), `aud`
    # if `jwt_audience` is set, and `iss` if `jwt_issuer` is set. When unset, the
    # service STILL refuses to start unless `allow_unverified_jwt=true` is
    # explicitly opted in — the prior `verify_signature=False` default let a
    # forged JWT drain another user's hourly budget counter before Go had a
    # chance to reject the work, so the dev-only escape hatch must be
    # intentional, not accidental.
    jwt_secret: str | None = None
    jwt_algorithm: str = "HS256"
    jwt_audience: str | None = None
    jwt_issuer: str | None = None
    allow_unverified_jwt: bool = False

    @field_validator("jwt_secret")
    @classmethod
    def _reject_short_secret(cls, v: str | None) -> str | None:
        """Mirror Go's auth.MinSecretBytes=32 — refuse keys shorter than 32 bytes
        when one IS configured (an empty/unset value is fine; the allow_unverified_jwt
        opt-in is the dev path)."""
        if v and len(v.encode("utf-8")) < 32:
            raise ValueError(
                f"PYTHON_AGENT_JWT_SECRET is {len(v)} bytes; minimum is 32 (auth.MinSecretBytes, RFC 7518 §3.2)"
            )
        return v

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