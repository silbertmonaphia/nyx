"""Planner node — one LLM call returning a JSON Plan.

Validates the response via `Plan.model_validate_json(...)`. Rejects oversized plans
(> settings.plan_max_steps), duplicate step ids, unknown step kinds. Synthesizes
the gaps the model cannot be trusted to fill: an empty plan becomes a single
direct-answer `llm` step, and a plan that ends on a data step (rag/tool) gets a
final answering `llm` step appended.
"""

from __future__ import annotations

import json
import logging

from langchain_core.messages import HumanMessage, SystemMessage
from langchain_openai import ChatOpenAI
from opentelemetry import trace

from .config import Settings
from .state import AgentState
from .types import (
    DoneEvent,
    ErrorEvent,
    Event,
    Plan,
    PlanEvent,
    PlanStep,
    StepKind,
    TerminatorEvent,
)

log = logging.getLogger("nyx.agent.planner")

PLANNER_SYSTEM_PROMPT = """You are a planner that decomposes a user's chat request into an ordered sequence of agent steps. The steps are executed sequentially by an orchestrator that supports four kinds:

- rag:    vector retrieval over the user's feed corpus; { query: <str> }
- tool:   call a registered tool by name; { tool: <name>, args: <object> }
- llm:    a single LLM completion; { prompt: <str> }
- react:  a nested model-tool-model loop, capped by max_iterations; { tool: <name>, args: <object>, max_iterations: <int> }

Return ONLY a JSON object matching this schema:
{{"steps": [{{"id": "<step_id>", "kind": "rag|tool|llm|react", "tool": "<name>", "args": {{...}}, "prompt": "<text>", "query": "<text>", "max_iterations": <int>}}, ...]}}

Rules:
- step `id` MUST be unique within the plan and stable across the run
- plan for only the MOST RECENT user message; earlier messages are context, never re-answer them
- `rag` and `tool` steps gather data and emit NO user-visible text; the plan MUST end with an `llm` step that writes the final answer using the data gathered by earlier steps
- the plan MUST contain at least one step; for greetings, small talk, or anything needing no retrieval or tools, emit exactly one `llm` step that answers the user
- prefer the minimum number of steps; don't insert filler
- use `rag` only when the request is clearly about the user's own data
- when unsure, prefer a single `llm` step
"""

# Sentinel JSON markers we accept when the upstream wraps JSON in fences.
_FENCE_PREFIX = "```"
_FENCE_SUFFIX = "```"


async def plan_node(state: AgentState, settings: Settings, tracer: trace.Tracer) -> AgentState:
    """Invoke the planner LLM; mutate state.plan and state.events_so_far."""
    with tracer.start_as_current_span("agent.step.plan") as span:
        span.set_attribute("step_id", "plan")
        span.set_attribute("kind", "plan")
        span.set_attribute("iteration", 0)

        client = ChatOpenAI(
            base_url=settings.llm_base_url,
            api_key=settings.llm_api_key or "no-key",
            model=settings.planner_model,
            temperature=0.0,
            max_tokens=512,
        )

        try:
            resp = await client.ainvoke(
                [
                    SystemMessage(content=PLANNER_SYSTEM_PROMPT),
                    HumanMessage(
                        content=_messages_to_prompt(state.get("messages", [])),
                    ),
                ]
            )
        except Exception as exc:  # noqa: BLE001 — surface as a typed error event
            return _emit_error(
                state,
                code="planner_failed",
                detail="Plan generation failed",
                reason=repr(exc),
            )

        raw = _extract_text(resp)
        plan = _parse_and_validate(raw, max_steps=settings.plan_max_steps)
        if isinstance(plan, _PlanError):
            return _emit_error(state, code=plan.code, detail=plan.detail, reason=plan.reason)

        state["plan"] = plan
        state["events_so_far"].append(
            Event(kind="plan", payload=PlanEvent(steps=plan.steps).model_dump())
        )
        return state


# --- helpers -------------------------------------------------------------


class _PlanError:
    def __init__(self, code: str, detail: str, reason: str = "") -> None:
        self.code = code
        self.detail = detail
        self.reason = reason


def _messages_to_prompt(messages: list) -> str:
    parts = []
    for m in messages:
        role = m.role if hasattr(m, "role") else m.get("role", "user")
        content = m.content if hasattr(m, "content") else m.get("content", "")
        if content:
            parts.append(f"[{role}] {content}")
    return "\n".join(parts) or "(no prior messages)"


def _extract_text(resp) -> str:
    if isinstance(resp.content, str):
        return resp.content
    if isinstance(resp.content, list):
        for block in resp.content:
            if isinstance(block, dict) and block.get("type") == "text":
                return block["text"]
            if hasattr(block, "text"):
                return block.text
    return str(resp.content)


def _parse_and_validate(raw: str, max_steps: int) -> Plan | _PlanError:
    text = raw.strip()
    if text.startswith(_FENCE_PREFIX):
        # Strip ```json ... ```
        lines = text.splitlines()
        lines = [ln for ln in lines if not ln.startswith(_FENCE_PREFIX)]
        text = "\n".join(lines).strip()

    try:
        data = json.loads(text)
    except json.JSONDecodeError as exc:
        return _PlanError("plan_invalid", "Plan is invalid", repr(exc))

    if not isinstance(data, dict) or "steps" not in data:
        return _PlanError("plan_invalid", "Plan is invalid", "missing 'steps' field")

    steps = data["steps"]
    if not isinstance(steps, list):
        return _PlanError("plan_invalid", "Plan is invalid", "'steps' is not a list")
    if len(steps) == 0:
        # Small models read "prefer the minimum number of steps" literally
        # and return {"steps": []} for greetings (deterministic on
        # Qwen3-4B for "hi"). A zero-step plan would end the run with NO
        # user-visible answer — the orchestrator's only reply mechanism is
        # an `llm` step's delta frames — so synthesize the canonical
        # direct-answer step instead of failing the run. The executor
        # appends the conversation to the step prompt, so the model sees
        # the actual message.
        steps = [
            {
                "id": "step_1",
                "kind": "llm",
                "prompt": "Answer the user's message directly.",
            }
        ]
    if len(steps) > max_steps:
        return _PlanError("plan_too_large", "Plan exceeds maximum size", f"{len(steps)} > {max_steps}")

    seen_ids: set[str] = set()
    parsed_steps: list[PlanStep] = []
    try:
        for s in steps:
            parsed = PlanStep.model_validate(s)
            if parsed.id in seen_ids:
                return _PlanError("plan_invalid", "Plan is invalid", f"duplicate step id {parsed.id!r}")
            seen_ids.add(parsed.id)
            parsed_steps.append(parsed)

        # Deterministic fallback, symmetric with the empty-plan synthesis
        # above: rag/tool steps gather data but emit NO user-visible text.
        # A plan whose last step is a data step finishes with nothing to
        # show even though the run "succeeds" (2026-10 "summarize my
        # feeds" bug). The system prompt asks the model to end with an
        # `llm` step, but a small model's compliance is not a guarantee —
        # enforce it here. `react` also produces text, so it qualifies as
        # the answering step. (Deliberately not counted against
        # max_steps: it guarantees one visible answer, and the model's
        # own output was already capped above.)
        if parsed_steps[-1].kind.value not in ("llm", "react"):
            answer_id = "final_answer"
            n = 2
            while answer_id in seen_ids:
                answer_id = f"final_answer_{n}"
                n += 1
            # Never silent: the parser modifying the model's plan is
            # observable in the agent log (and, via the plan frame, on the
            # wire — the appended step is visible to the client).
            log.info(
                "planner: model plan ended with a %s step; appended answering step %r",
                parsed_steps[-1].kind.value,
                answer_id,
            )
            parsed_steps.append(
                PlanStep(
                    id=answer_id,
                    kind="llm",
                    prompt=(
                        "Write the final answer to the user's most recent "
                        "request, using the data gathered by earlier steps."
                    ),
                )
            )
        return Plan(steps=parsed_steps)
    except Exception as exc:  # noqa: BLE001
        return _PlanError("plan_invalid", "Plan is invalid", repr(exc))


def _emit_error(state: AgentState, code: str, detail: str, reason: str = "") -> AgentState:
    """Emit a typed error event then a done event then a terminator event.

    Wire shapes per `frontend/src/features/chat/services/chatService.ts:232-258, 185-215, 169-171`:
      - error:        `{"error": "<detail>", "code": "<code>", "request_id": "<rid>"}`
      - done:         `{"usage": {"prompt_tokens":0, "completion_tokens":0, "total_tokens":0}}`
      - terminator:   `{"reason": "<reason>"}` (legacy clients; frontend uses [DONE])
    """
    state["terminal_status"] = "failed"
    state["terminal_reason"] = code
    state["events_so_far"].append(
        Event(
            kind="error",
            payload=ErrorEvent(
                error=detail, code=code, request_id=state.get("request_id")
            ).model_dump(),
        )
    )
    state["events_so_far"].append(Event(kind="done", payload=DoneEvent().model_dump()))
    state["events_so_far"].append(
        Event(kind="terminator", payload=TerminatorEvent(reason=reason).model_dump())
    )
    return state