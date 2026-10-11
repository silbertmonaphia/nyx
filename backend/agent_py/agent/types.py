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
    ]
    payload: dict[str, Any] = Field(default_factory=dict)


# --- Budget guard --------------------------------------------------------


class BudgetDecision(BaseModel):
    decision: Literal["allow", "exceeded", "unavailable"]
    tokens_used: int
    tokens_remaining: int