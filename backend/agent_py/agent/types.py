"""Pydantic models — the wire format between the LangGraph orchestrator and the Go SSE proxy.

The Go chat handler already emits 10 SSE event kinds; this module mirrors them so the Python
orchestrator produces an identical envelope. Field names match the Go SSE wire byte-for-byte.
"""

from __future__ import annotations

from enum import Enum
from typing import Any, Literal

from pydantic import BaseModel, Field

# --- Steps / Plans --------------------------------------------------------


class StepKind(str, Enum):
    rag = "rag"
    tool = "tool"
    llm = "llm"
    react = "react"


class ToolCall(BaseModel):
    """Matches Go `llm.ToolCall` wire — `arguments` is raw JSON, not a parsed dict."""

    id: str
    name: str
    arguments: str


class PlanStep(BaseModel):
    id: str
    kind: StepKind
    tool: str | None = None
    # `args` MUST stay a plain free-form object (`dict[str, Any] | None`).
    # The planner binds `Plan` as the provider's `response_format`
    # `json_schema`; extra schema branches (e.g. a `BaseModel` alternative,
    # which renders as an always-satisfiable empty-object branch) let
    # guided decoders silently resolve `args` to `{}` — dropping tool
    # arguments. Per-step arg schemas can land later by changing this
    # annotation alone; the planner is schema-driven.
    args: dict[str, Any] | None = None
    prompt: str | None = None
    query: str | None = None
    max_iterations: int | None = None


class Plan(BaseModel):
    steps: list[PlanStep]


# --- Chat request shape --------------------------------------------------


class ChatMessage(BaseModel):
    role: Literal["system", "user", "assistant", "tool"]
    content: str | None = None
    tool_call_id: str | None = None
    name: str | None = None
    tool_calls: list[ToolCall] | None = None


class ChatRequest(BaseModel):
    messages: list[ChatMessage]
    agent: str | None = None
    model: str | None = None
    run_id: str | None = None  # server-minted uuid4; client echoes for resume


# --- SSE event payloads (mirror Go chat handler envelope) -----------------


class PlanEvent(BaseModel):
    steps: list[PlanStep]


class StepStartEvent(BaseModel):
    step_id: str
    kind: StepKind


class ToolCallEvent(BaseModel):
    step_id: str
    call_id: str
    name: str
    args: dict[str, Any]


class ToolResultEvent(BaseModel):
    step_id: str
    call_id: str
    ok: bool
    result: Any | None = None
    error: str | None = None
    # Additive retry counter (openspec/changes/agent-tool-dispatch-retry §3.2).
    # 0 on a first-try success; N when N retries were made for this dispatch.
    # Intermediate failure frames carry the retries made *so far* (0 for the
    # first-attempt failure); the terminal failure frame after retry-budget
    # exhaustion carries the full `tool_retry_max`. Old consumers ignore
    # unknown fields — `chatService.ts` reads it only when present.
    retry_count: int = 0


class StepEndEvent(BaseModel):
    step_id: str
    status: Literal["ok", "failed"]
    reason: str | None = None


class DeltaEvent(BaseModel):
    """Token delta — wire field `delta` per `frontend/src/features/chat/services/chatService.ts:176`."""

    delta: str


class NoteEvent(BaseModel):
    text: str
    provider: str | None = None
    # Additive fields for run-state resume (FUTURE_AGENT.md §1.1).
    # `partial_failure` is the agent-tool-dispatch-retry variant: emitted
    # immediately before the final answer's deltas when the run diverged
    # from the plan (see specs/agent-partial-failure/spec.md).
    kind: Literal["resumed", "fresh_run", "partial_failure"] | None = None
    last_step_id: str | None = None
    run_id: str | None = None
    # Populated only when `kind == "partial_failure"`: the step ids whose
    # terminal status diverged from the plan. Empty when the run failed
    # before any step reached execution (plan-level errors).
    failed_steps: list[str] = Field(default_factory=list)


class DoneUsage(BaseModel):
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0


class DoneEvent(BaseModel):
    """Terminal frame before `[DONE]` — wire shape `{"usage": {...}}` per `chatService.ts:185-215`."""

    usage: DoneUsage = DoneUsage()


class ErrorEvent(BaseModel):
    """Error frame — wire shape `{"error","code","request_id"}` per `chatService.ts:232-258`."""

    error: str
    code: str | None = None
    request_id: str | None = None


# Error codes that mean the run diverged from the plan before/without any
# step completing — the CLOSED trigger set for the partial-failure note
# (openspec/changes/agent-tool-dispatch-retry » agent-partial-failure).
# Deliberately absent: `run_metadata_unavailable` (non-fatal per
# agent-run-state), `planner_failed` (a provider/construction fault — the
# run never had a plan to diverge from; round 2, NEW-2), `empty_answer`,
# `agent_unavailable`. Shared by `planner._emit_error` and
# `executor._has_plan_level_error` so the two sides cannot drift.
PLAN_LEVEL_ERROR_CODES = frozenset(
    {
        "plan_too_large",
        "plan_invalid",
        "react_iterations_exceeded",
        "budget_exceeded",
        "budget_unavailable",
    }
)


class RunIDEvent(BaseModel):
    """First frame of a fresh run — server mints a `run_id` and echoes it back.

    Client SHOULD persist this and echo it on the next request to enable
    resume after backend/Python restart (see `agent-run-state` spec).
    """

    run_id: str


class TerminatorEvent(BaseModel):
    """Final frame. Frontend parser doesn't read this kind — `[DONE]` triggers terminator — but
    we emit it so old clients that listen on `event: terminator` keep working."""

    reason: str | None = None


# Wire Event union — payload is the typed event object that renders flat to the SSE `data:`
# line (no `{kind, data}` wrapper; the kind is in the SSE `event:` field).
class Event(BaseModel):
    kind: Literal[
        "plan",
        "step_start",
        "tool_call",
        "tool_result",
        "step_end",
        "delta",
        "done",
        "note",
        "error",
        "terminator",
        "run_id",  # new — server-minted run_id (agent-run-state)
    ]
    payload: dict[str, Any] = Field(default_factory=dict)


# --- Budget guard --------------------------------------------------------


class BudgetDecision(BaseModel):
    decision: Literal["allow", "exceeded", "unavailable"]
    tokens_used: int
    tokens_remaining: int