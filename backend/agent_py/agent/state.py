"""LangGraph state shape.

AgentState flows through the planner → executor nodes. Each node appends typed events to
`events_so_far`; the SSE emitter (chunk B / task 8.1) yields them as they arrive.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypedDict

from langchain_core.messages import BaseMessage

from .types import ChatMessage, Event, LLMOverrideView, Plan, TavilyOverrideView


class AgentState(TypedDict, total=False):
    # --- Input (set by FastAPI handler before invoking the graph) ---
    user_id: int
    run_id: str
    request_id: str | None  # from the HTTP request; threaded into error events
    messages: list[ChatMessage]
    model: str | None  # model override; None uses settings.llm_model
    llm_override: LLMOverrideView | None
    tavily_override: TavilyOverrideView | None

    # --- Plan + per-step output (filled by planner + executor) ---
    plan: Plan | None
    step_outputs: dict[str, Any]
    cumulative_tokens: int
    cumulative_prompt_tokens: int
    cumulative_completion_tokens: int

    # --- Tool-dispatch retry budget (agent-tool-dispatch-retry) ---
    # Per-(step, tool) retry consumption: step_id → tool name → retry
    # attempts consumed in that step. Keyed by step id, so a fresh step
    # naturally starts with a fresh budget while ReAct iterations within
    # one step share their tool's budget (design.md §Open Questions,
    # resolved 2026-10-13: per-step, per-tool across the entire react step).
    tool_retry_used: dict[str, dict[str, int]]
    # step_id → tool names whose dispatch ended in a TERMINAL failure
    # (exhaustion, non-retryable, or the model declined to correct).
    # Distinct from `tool_retry_used`: consuming the full allowance and
    # then RECOVERING on the last allowed attempt is not a terminal
    # failure and must not flag the step (test-review round 2, NEW-1).
    # Feeds the partial-failure banner trigger in `_collect_failed_steps`.
    tool_terminal_failures: dict[str, list[str]]

    # --- Event stream (replayed to the SSE emitter per node completion) ---
    # NOTE: when a checkpointer is wired (production), ALL channels —
    # including this one — are persisted into `langgraph.*` on every node
    # transition; that's how a resumed run replays already-emitted frames.
    # Consequence: checkpoint size grows with the event count. Acceptable
    # while plans are small; langGraph retention prunes old checkpoints.
    events_so_far: list[Event]

    # --- Terminal markers ---
    terminal_status: str  # "ok" | "failed" | "budget_exceeded" | "budget_unavailable"
    terminal_reason: str | None


def new_state(
    *,
    user_id: int,
    run_id: str,
    messages: list[ChatMessage],
    model: str | None,
    request_id: str | None = None,
    llm_override: LLMOverrideView | None = None,
    tavily_override: TavilyOverrideView | None = None,
) -> AgentState:
    return AgentState(
        user_id=user_id,
        run_id=run_id,
        request_id=request_id,
        messages=messages,
        model=model,
        llm_override=llm_override,
        tavily_override=tavily_override,
        plan=None,
        step_outputs={},
        cumulative_tokens=0,
        cumulative_prompt_tokens=0,
        cumulative_completion_tokens=0,
        tool_retry_used={},
        tool_terminal_failures={},
        events_so_far=[],
        terminal_status="ok",
        terminal_reason=None,
    )


def _int_or_zero(value: Any) -> int:
    return value if isinstance(value, int) and not isinstance(value, bool) else 0


def _add_usage(
    state: AgentState, prompt: int, completion: int, total: int
) -> tuple[int, int, int]:
    state["cumulative_prompt_tokens"] = state.get("cumulative_prompt_tokens", 0) + prompt
    state["cumulative_completion_tokens"] = (
        state.get("cumulative_completion_tokens", 0) + completion
    )
    state["cumulative_tokens"] = state.get("cumulative_tokens", 0) + total
    return prompt, completion, total


def usage_from_message(message: BaseMessage) -> tuple[int, int, int]:
    """Return (prompt, completion, total) token counts from a model response.

    Prefers the standardized `usage_metadata` (langchain-core ≥ 0.2.2);
    falls back to the OpenAI-shaped `response_metadata["token_usage"]`.
    Stubs and providers that omit usage yield zeros.
    """
    meta = getattr(message, "usage_metadata", None)
    if isinstance(meta, dict):
        prompt = _int_or_zero(meta.get("input_tokens"))
        completion = _int_or_zero(meta.get("output_tokens"))
        if prompt or completion:
            total = meta.get("total_tokens")
            return prompt, completion, _int_or_zero(total) or prompt + completion
    wire = getattr(message, "response_metadata", None) or {}
    token_usage = wire.get("token_usage")
    wire = token_usage if isinstance(token_usage, dict) else {}
    prompt = _int_or_zero(wire.get("prompt_tokens"))
    completion = _int_or_zero(wire.get("completion_tokens"))
    total = wire.get("total_tokens")
    return prompt, completion, _int_or_zero(total) or prompt + completion


def accumulate_usage(state: AgentState, message: BaseMessage) -> tuple[int, int, int]:
    """Add one model response's real usage to the state's cumulative counters.

    Returns the (prompt, completion, total) delta that was added. `.get`
    defaults keep runs resumed from pre-existing checkpoints (channels
    without the new keys) working.
    """
    prompt, completion, total = usage_from_message(message)
    return _add_usage(state, prompt, completion, total)


def accumulate_usage_metadata(
    state: AgentState, meta: Mapping[str, Any] | None
) -> tuple[int, int, int]:
    """Accumulate a raw `usage_metadata` dict (e.g. from a callback handler)."""
    meta = meta if isinstance(meta, Mapping) else {}
    prompt = _int_or_zero(meta.get("input_tokens"))
    completion = _int_or_zero(meta.get("output_tokens"))
    total = _int_or_zero(meta.get("total_tokens")) or prompt + completion
    return _add_usage(state, prompt, completion, total)
