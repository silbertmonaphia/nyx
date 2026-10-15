"""LangGraph state shape.

AgentState flows through the planner → executor nodes. Each node appends typed events to
`events_so_far`; the SSE emitter (chunk B / task 8.1) yields them as they arrive.
"""

from __future__ import annotations

from typing import Any, TypedDict

from .types import ChatMessage, Event, Plan


class AgentState(TypedDict, total=False):
    # --- Input (set by FastAPI handler before invoking the graph) ---
    user_id: int
    run_id: str
    request_id: str | None  # from the HTTP request; threaded into error events
    messages: list[ChatMessage]
    model: str | None  # model override; None uses settings.llm_model

    # --- Plan + per-step output (filled by planner + executor) ---
    plan: Plan | None
    step_outputs: dict[str, Any]
    cumulative_tokens: int

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
) -> AgentState:
    return AgentState(
        user_id=user_id,
        run_id=run_id,
        request_id=request_id,
        messages=messages,
        model=model,
        plan=None,
        step_outputs={},
        cumulative_tokens=0,
        tool_retry_used={},
        tool_terminal_failures={},
        events_so_far=[],
        terminal_status="ok",
        terminal_reason=None,
    )