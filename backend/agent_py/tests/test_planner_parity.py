"""Cross-provider parity tests — the same canned plan replayed through the REAL
OpenAI-compatible wire for both an OpenAI-shaped and a vLLM-shaped response.

Unlike a mock that hands back a `Plan` object directly, these tests drive the
installed `langchain-openai` client over `httpx.MockTransport`, so the actual
request body (`response_format: json_schema`) and the actual response parser
are exercised. A provider wire divergence (e.g. a response carrying
`tool_calls` instead of parsed content) surfaces here, not in production.
"""

from __future__ import annotations

import json

import httpx
from langchain_openai import ChatOpenAI

from agent.config import Settings
from agent.planner import _emit_structured_plan
from agent.state import new_state
from agent.types import Plan, PlanEvent

CANNED_STEPS = [
    {
        "id": "tool_1",
        "kind": "tool",
        "tool": "list_feeds",
        "args": {"name": "alpha", "limit": 10, "filters": ["recent", "unread"]},
    },
    {"id": "llm_1", "kind": "llm", "prompt": "Summarize the result."},
]
CANNED_CONTENT = json.dumps({"steps": CANNED_STEPS})


def _await(coro):
    import asyncio

    return asyncio.run(coro)


def _settings(monkeypatch, **overrides):
    monkeypatch.setenv("PYTHON_AGENT_LLM_BASE_URL", "http://parity.test/v1")
    monkeypatch.setenv("PYTHON_AGENT_LLM_API_KEY", "test-key")
    monkeypatch.setenv("PYTHON_AGENT_LLM_MODEL", "test-model")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED", "true")
    monkeypatch.setenv("PYTHON_AGENT_PLANNER_RETRY_MAX", "1")
    monkeypatch.setenv("PYTHON_AGENT_PLAN_MAX_STEPS", "5")
    for k, v in overrides.items():
        monkeypatch.setenv(f"PYTHON_AGENT_{k}", v)
    return Settings()


def _chat_completion(content, *, extra=None):
    body = {
        "id": "chatcmpl-parity",
        "object": "chat.completion",
        "created": 0,
        "model": "parity-model",
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": content},
                "finish_reason": "stop",
            }
        ],
        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
    }
    if extra:
        body.update(extra)
    return body


class _Wire:
    """MockTransport handler: serves canned response bodies and records request bodies."""

    def __init__(self, responses: list[dict]):
        self.requests: list[dict] = []
        self._responses = list(responses)

    def handler(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(json.loads(request.content))
        if not self._responses:
            raise AssertionError("provider called more times than scripted")
        return httpx.Response(200, json=self._responses.pop(0))


def _patch_provider(monkeypatch, wire: _Wire) -> None:
    """Route agent.planner's ChatOpenAI instances through MockTransport."""
    transport = httpx.MockTransport(wire.handler)
    async_client = httpx.AsyncClient(transport=transport)
    real = ChatOpenAI

    def factory(**kwargs):
        kwargs["http_async_client"] = async_client
        return real(**kwargs)

    monkeypatch.setattr("agent.planner.ChatOpenAI", factory)


class _NoopTracer:
    def start_as_current_span(self, name, **_attrs):
        from contextlib import nullcontext

        span = type("Span", (), {"set_attribute": lambda self, k, v: None})()
        return nullcontext(span)


def _state():
    return new_state(
        user_id=1,
        run_id="parity",
        model="test-model",
        messages=[{"role": "user", "content": "hello"}],
    )


def _expected_plan() -> Plan:
    return Plan.model_validate({"steps": CANNED_STEPS})


def test_openai_replay(monkeypatch):
    """Canned plan arrives through the real OpenAI-shaped wire."""
    settings = _settings(monkeypatch)
    wire = _Wire([_chat_completion(CANNED_CONTENT)])
    _patch_provider(monkeypatch, wire)

    plan_or_err = _await(_emit_structured_plan(_state(), settings, _NoopTracer()))

    assert isinstance(plan_or_err, Plan), plan_or_err
    assert plan_or_err.model_dump() == _expected_plan().model_dump()

    # The request pinned json_schema AND non-strict mode — strict mode cannot
    # express the free-form `args` object (see planner._build_structured_client).
    sent = wire.requests[0]
    rfmt = sent["response_format"]
    assert rfmt["type"] == "json_schema"
    assert not rfmt["json_schema"].get("strict"), (
        "strict must be off so free-form tool args stay representable"
    )
    schema_text = json.dumps(rfmt["json_schema"]["schema"])
    assert "steps" in schema_text
    # The load-bearing consequence of non-strict mode: free-form `args`
    # stays an open object (strict mode forces `additionalProperties: false`,
    # i.e. `args` could only ever be `{}`).
    assert "additionalProperties" in schema_text


def test_vllm_replay(monkeypatch):
    """Same canned plan through a vLLM-shaped response envelope (vendor extras
    the OpenAI client must tolerate)."""
    settings = _settings(monkeypatch)
    vllm_body = _chat_completion(
        CANNED_CONTENT,
        extra={
            "system_fingerprint": None,
            "usage": {
                "prompt_tokens": 1,
                "completion_tokens": 1,
                "total_tokens": 2,
                "prompt_tokens_details": None,
            },
        },
    )
    wire = _Wire([vllm_body])
    _patch_provider(monkeypatch, wire)

    plan_or_err = _await(_emit_structured_plan(_state(), settings, _NoopTracer()))

    assert isinstance(plan_or_err, Plan), plan_or_err
    assert plan_or_err.model_dump() == _expected_plan().model_dump()


def test_tool_calls_wire_divergence_recovers_on_retry(monkeypatch):
    """A provider response carrying `tool_calls` and no content is the
    historical vLLM tool-call wire divergence. Against the real bound chain
    this surfaces as an `OutputParserException` ("Invalid json output"); the
    planner must treat it as a retryable conformance failure — re-prompt
    with feedback and recover on the next attempt — not crash the graph
    node. (The non-`Plan`-return variant of this class is covered in
    test_planner.py::test_non_plan_return_is_retryable_and_recovers.)"""
    settings = _settings(monkeypatch)
    tool_call_body = _chat_completion(None)
    tool_call_body["choices"][0]["message"]["tool_calls"] = [
        {
            "id": "call_1",
            "type": "function",
            "function": {"name": "list_feeds", "arguments": "{}"},
        }
    ]
    wire = _Wire([tool_call_body, _chat_completion(CANNED_CONTENT)])
    _patch_provider(monkeypatch, wire)

    plan_or_err = _await(_emit_structured_plan(_state(), settings, _NoopTracer()))

    assert isinstance(plan_or_err, Plan), plan_or_err
    assert len(wire.requests) == 2
    # The retry's prompt carries the conformance feedback.
    retry_text = json.dumps(wire.requests[1]["messages"])
    assert "PREVIOUS_ATTEMPT_FAILED" in retry_text


def test_plan_frame_identical_across_paths(monkeypatch):
    """Structured and legacy paths produce byte-identical `plan` frame payloads
    for the same model output (README claims byte-identity; this pins it)."""
    from agent.planner import _parse_and_validate

    settings = _settings(monkeypatch)
    wire = _Wire([_chat_completion(CANNED_CONTENT)])
    _patch_provider(monkeypatch, wire)

    structured = _await(_emit_structured_plan(_state(), settings, _NoopTracer()))
    legacy = _parse_and_validate(CANNED_CONTENT, max_steps=5)

    assert isinstance(structured, Plan) and isinstance(legacy, Plan)
    assert (
        PlanEvent(steps=structured.steps).model_dump()
        == PlanEvent(steps=legacy.steps).model_dump()
    )
