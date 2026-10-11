"""LangGraph state shape.

AgentState flows through the planner → executor nodes. Each node appends typed events to
`events_so_far`; the SSE emitter (chunk B / task 8.1) yields them as they arrive.
"""

from __future__ import annotations

from typing import Any, TypedDict

from .types import (
    ChatMessage,
    DoneEvent,
    ErrorEvent,
    Event,
    NoteEvent,
    Plan,
    PlanEvent,
    StepEndEvent,
    StepStartEvent,
    TerminatorEvent,
)


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

    # --- Event stream (consumed by SSE emitter; never persisted) ---
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
        events_so_far=[],
        terminal_status="ok",
        terminal_reason=None,
    )