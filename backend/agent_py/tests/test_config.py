"""Tests for Settings validation — particularly the OpenAI-key
guard that refuses to start when targeting api.openai.com without
a key (spec scenario "Required key missing refuses startup")."""

from __future__ import annotations

import pytest

from agent.config import Settings


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
