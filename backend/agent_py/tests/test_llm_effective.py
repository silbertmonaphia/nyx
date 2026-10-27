"""Per-run BYOK effective config — tasks 5.1/5.2 of
openspec/changes/add-user-llm-byok.

The Go chat handler forwards the user's config in the `/agent/run`
payload. This module pins the consumer side of that wire contract
(through the captured Go fixture), the request/state split that keeps
plaintext keys out of the checkpointed state (D11), and the resolution
precedence every client-construction site relies on.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from langgraph.checkpoint.serde.jsonplus import JsonPlusSerializer

from agent.config import Settings
from agent.llm_effective import (
    NO_KEY_PLACEHOLDER,
    OverrideSecrets,
    llm_holder_key,
    override_only_state,
    resolve_llm,
    resolve_llm_endpoint,
    run_overrides_from_request,
    structured_supported,
)
from agent.state import new_state
from agent.types import (
    ChatMessage,
    ChatRequest,
    LLMOverride,
    LLMOverrideView,
    TavilyOverride,
    TavilyOverrideView,
)

FIXTURE = (
    Path(__file__).resolve().parents[3]
    / "frontend"
    / "tests"
    / "e2e"
    / "fixtures"
    / "agent-payload-llm-config.json"
)

_HOLDER_KEY = "holder-key-not-in-state"
_DEPLOYMENT_KEY = "deployment-key"
_VIEW = LLMOverrideView(
    handle="handle-1", base_url="http://user-llm.test/v1", model="user-model"
)


def _settings(**overrides) -> Settings:
    base = {
        "_env_file": None,
        "llm_base_url": "http://deployment-llm.test/v1",
        "llm_api_key": _DEPLOYMENT_KEY,
        "llm_model": "deployment-model",
        "llm_planner_model": "planner-model",
    }
    base.update(overrides)
    return Settings(**base)


def _request(**overrides) -> ChatRequest:
    payload = {"messages": [{"role": "user", "content": "hi"}]}
    payload.update(overrides)
    return ChatRequest.model_validate(payload)


def _state(view: LLMOverrideView | None = _VIEW, model: str | None = None):
    return new_state(
        user_id=1,
        run_id="run-1",
        messages=[ChatMessage(role="user", content="hi")],
        model=model,
        llm_override=view,
        tavily_override=(
            TavilyOverrideView(handle="handle-1", base_url="http://tavily.test")
            if view
            else None
        ),
    )


def _serialize_state(state: dict) -> bytes:
    serde = JsonPlusSerializer(
        allowed_msgpack_modules=[
            ("agent.types", "Plan"),
            ("agent.types", "ChatMessage"),
            ("agent.types", "Event"),
            ("agent.types", "LLMOverrideView"),
            ("agent.types", "TavilyOverrideView"),
        ]
    )
    _type, data = serde.dumps_typed(state)
    return data


def test_go_payload_fixture_parses_through_chat_request():
    payload = json.loads(FIXTURE.read_text(encoding="utf-8"))

    request = ChatRequest.model_validate(payload)

    assert request.llm_config is not None
    assert request.llm_config.base_url == "http://localhost:8000/v1"
    assert request.llm_config.model == "Qwen/Qwen3-4B-Instruct-2507-FP8"
    assert request.llm_config.api_key == "e2e-placeholder-key-not-secret"
    assert request.tavily_config is not None
    assert request.tavily_config.base_url == "https://api.tavily.com/search"
    assert request.tavily_config.api_key == "e2e-placeholder-key-not-secret"


def test_absent_configs_default_to_none():
    request = _request()

    assert request.llm_config is None
    assert request.tavily_config is None


def test_request_split_keeps_the_key_out_of_the_state_views():
    request = _request(
        llm_config={
            "base_url": "http://user.test/v1",
            "api_key": _HOLDER_KEY,
            "model": "user-model",
        },
        tavily_config={
            "base_url": "https://user-tavily.test/search",
            "api_key": _HOLDER_KEY,
        },
    )

    secrets, llm_view, tavily_view = run_overrides_from_request(request)

    assert isinstance(secrets, OverrideSecrets)
    assert secrets.llm_api_key == _HOLDER_KEY
    assert secrets.tavily_api_key == _HOLDER_KEY
    assert llm_view is not None and tavily_view is not None
    assert llm_view.handle == tavily_view.handle == secrets.handle
    assert llm_view.base_url == "http://user.test/v1"
    assert llm_view.model == "user-model"
    assert tavily_view.base_url == "https://user-tavily.test/search"
    assert "api_key" not in llm_view.model_dump()
    assert "api_key" not in tavily_view.model_dump()
    assert not hasattr(llm_view, "api_key")


def test_serialized_state_carries_no_key_material():
    request = _request(
        llm_config={
            "base_url": "http://user.test/v1",
            "api_key": _HOLDER_KEY,
            "model": "user-model",
        },
        tavily_config={
            "base_url": "https://user-tavily.test/search",
            "api_key": _HOLDER_KEY,
        },
    )
    _secrets, llm_view, tavily_view = run_overrides_from_request(request)
    state = new_state(
        user_id=1,
        run_id="run-1",
        messages=[ChatMessage(role="user", content="hi")],
        model=None,
        llm_override=llm_view,
        tavily_override=tavily_view,
    )

    blob = _serialize_state(state)

    assert _HOLDER_KEY.encode() not in blob
    assert b"api_key" not in blob
    assert b"user-model" in blob
    assert b"http://user.test/v1" in blob


def test_holder_is_the_only_key_source_for_an_override():
    state = _state()
    state["api_key"] = "rogue-state-key"

    effective = resolve_llm(
        state, _settings(), planner=False, api_key_from_holder=None
    )

    assert effective.api_key == NO_KEY_PLACEHOLDER
    assert effective.api_key != _DEPLOYMENT_KEY
    assert effective.base_url == "http://user-llm.test/v1"


def test_llm_holder_key_reads_only_the_holder():
    assert llm_holder_key(None) is None
    assert llm_holder_key(OverrideSecrets(handle="h")) is None
    assert llm_holder_key(OverrideSecrets(handle="h", llm_api_key="k")) == "k"


def test_planner_override_wins_endpoint_model_and_holder_key():
    effective = resolve_llm(
        _state(), _settings(), planner=True, api_key_from_holder=_HOLDER_KEY
    )

    assert effective.base_url == "http://user-llm.test/v1"
    assert effective.model == "user-model"
    assert effective.api_key == _HOLDER_KEY
    assert effective.user_supplied is True


def test_planner_without_override_is_settings_exactly():
    effective = resolve_llm(
        _state(view=None),
        _settings(),
        planner=True,
        api_key_from_holder=_HOLDER_KEY,
    )

    assert effective.base_url == "http://deployment-llm.test/v1"
    assert effective.model == "planner-model"
    assert effective.api_key == _DEPLOYMENT_KEY
    assert effective.user_supplied is False


def test_stale_checkpoint_handle_fails_closed():
    effective = resolve_llm(
        _state(),
        _settings(),
        planner=False,
        api_key_from_holder=_HOLDER_KEY,
        holder_handle="a-different-run",
    )

    assert effective.base_url == "http://user-llm.test/v1"
    assert effective.api_key == NO_KEY_PLACEHOLDER


def test_matching_handle_uses_the_holder_key():
    effective = resolve_llm(
        _state(),
        _settings(),
        planner=False,
        api_key_from_holder=_HOLDER_KEY,
        holder_handle="handle-1",
    )

    assert effective.api_key == _HOLDER_KEY


def test_executor_override_model_beats_settings_model():
    effective = resolve_llm(
        _state(), _settings(), planner=False, api_key_from_holder=_HOLDER_KEY
    )

    assert effective.model == "user-model"
    assert effective.base_url == "http://user-llm.test/v1"


def test_executor_state_model_beats_override_model():
    effective = resolve_llm(
        _state(model="request-model"),
        _settings(),
        planner=False,
        api_key_from_holder=_HOLDER_KEY,
    )

    assert effective.model == "request-model"
    assert effective.base_url == "http://user-llm.test/v1"


def test_executor_without_override_defaults_to_settings_model():
    effective = resolve_llm(
        _state(view=None),
        _settings(),
        planner=False,
        api_key_from_holder=_HOLDER_KEY,
    )

    assert effective.model == "deployment-model"
    assert effective.api_key == _DEPLOYMENT_KEY


def test_override_endpoint_resolution_returns_no_key():
    base_url, model = resolve_llm_endpoint(_state(), _settings(), planner=True)

    assert (base_url, model) == ("http://user-llm.test/v1", "user-model")
    assert _HOLDER_KEY not in (base_url, model)
    assert _DEPLOYMENT_KEY not in (base_url, model)


def test_override_only_state_matches_a_full_state_without_a_model_override():
    effective = resolve_llm(
        override_only_state(_VIEW),
        _settings(),
        planner=True,
        api_key_from_holder=_HOLDER_KEY,
    )

    assert effective == resolve_llm(
        _state(), _settings(), planner=True, api_key_from_holder=_HOLDER_KEY
    )


@pytest.mark.parametrize(
    "host_url",
    [
        "http://deepseek.test/v1",
        "https://api.deepseek.com/v1",
        "http://api.deepseek.com:8443/v1",
    ],
)
def test_structured_supported_rejects_deepseek_hosts(host_url):
    assert structured_supported(host_url, _settings()) is False


@pytest.mark.parametrize(
    "host_url",
    [
        "http://user-llm.test/v1",
        "http://localhost:8000/v1",
        "https://api.openai.com/v1",
    ],
)
def test_structured_supported_allows_other_hosts(host_url):
    assert structured_supported(host_url, _settings()) is True


def test_structured_supported_follows_the_global_flag_without_an_override():
    assert structured_supported(None, _settings()) is True
    assert (
        structured_supported(None, _settings(planner_structured_enabled=False))
        is False
    )


def test_structured_supported_global_flag_off_beats_a_supported_host():
    settings = _settings(planner_structured_enabled=False)

    assert structured_supported("http://user-llm.test/v1", settings) is False


def test_chat_request_accepts_plain_override_models():
    request = _request(
        llm_config=LLMOverride(
            base_url="http://x/v1", api_key="k", model="m"
        ).model_dump(),
        tavily_config=TavilyOverride(
            base_url="http://t", api_key="k"
        ).model_dump(),
    )

    assert request.llm_config is not None
    assert request.tavily_config is not None
