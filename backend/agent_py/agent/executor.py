"""Executor node — sequential over Plan.steps.

For each step runs the appropriate handler:
  - rag   → RagClient.retrieve → render context block into state.step_outputs[step.id]
  - tool  → ToolDispatcher.call (agent-native tool, else MCP) → render result
  - llm   → direct LLM call with state.messages + step.prompt (+ data from earlier rag/tool steps) → accumulate text deltas
  - react → nested subgraph (model → tool → model), capped by max_iterations

The executor is budget-aware: before every model call it consults BudgetGuard. Budget
exhaustion emits `error{code:"budget_exceeded"}` + done + terminator.
"""

from __future__ import annotations

import asyncio
import json
import logging
import random
import re
import uuid
from typing import Any

from langchain_core.messages import AIMessage, HumanMessage, SystemMessage, ToolMessage
from langchain_openai import ChatOpenAI
from opentelemetry import trace
from pydantic import SecretStr

from .approvals import (
    DECISION_APPROVE,
    DECISION_UNAVAILABLE,
    ApprovalGate,
    denial_text_for,
)
from .budget import BudgetGuard
from .config import Settings
from .observability import (
    agent_steps_total,
    agent_tokens_total,
    agent_tool_calls_total,
    agent_tool_retries_total,
    jaeger_span,
    trace_observation,
    update_observation,
)
from .rag_client import RagAuthError, RagClient, RagUnavailableError
from .state import AgentState
from .tool_dispatch import ToolDispatcher
from .tools_client import (
    ToolAuthError,
    ToolError,
    ToolRejectedError,
    ToolUnavailableError,
)
from .types import (
    PLAN_LEVEL_ERROR_CODES,
    DeltaEvent,
    DoneEvent,
    ErrorEvent,
    Event,
    NoteEvent,
    Plan,
    StepEndEvent,
    StepStartEvent,
    TerminatorEvent,
    ToolCallEvent,
    ToolResultEvent,
)

DEFAULT_ESTIMATED_TOKENS = 1000

log = logging.getLogger("nyx.agent.executor")

# Sentinel marker echoed into the tool-arg re-prompt; matches the planner's
# `PREVIOUS_ATTEMPT_FAILED` block so the model sees a consistent convention.
PREVIOUS_ATTEMPT_FAILED_HEADER = "PREVIOUS_ATTEMPT_FAILED"


async def executor_node(
    state: AgentState,
    *,
    settings: Settings,
    tracer: trace.Tracer,
    budget: BudgetGuard,
    rag: RagClient,
    tools: ToolDispatcher,
    approvals: ApprovalGate | None = None,
) -> AgentState:
    plan: Plan | None = state.get("plan")
    if plan is None:
        state["terminal_status"] = "failed"
        state["terminal_reason"] = "no_plan"
        # No partial-failure note here: plan-level failures are emitted by
        # the PLANNER's own terminal helper (`planner._emit_error`), in the
        # same node chunk as error/done — `run_agent_stream` slices
        # positionally, so a note inserted into the already-streamed
        # planner range from this node would never reach the wire
        # (test-review round 1, F1).
        return state
    # Defensive: the planner always emits a typed `Plan` instance after the
    # structured-output migration (openspec/changes/agent-planner-structured).
    # A dict here would be the only way for this assertion to fire — and
    # that would mean the planner regressed to its legacy dict-only path
    # without the executor-side machinery being updated to match.
    assert isinstance(plan, Plan), f"executor expected Plan, got {type(plan).__name__}"

    for step in plan.steps:
        # Resume-aware skip (absorbed into agent-stale-run-sweeper,
        # 2026-10-14 user-approved): a step whose output already exists in
        # the checkpointed state completed in a prior phase — re-running it
        # would double-spend tokens and re-invoke tools. Per the resume
        # contract its frames were emitted in the prior phase, so nothing
        # new is emitted for it here.
        if step.id in (state.get("step_outputs") or {}):
            continue
        with (
            # Langfuse twin of the OTel step span (agent-langfuse-tracing
            # 3.4): no-op when tracing is disabled or the client fails to
            # open the observation. Entered BEFORE the Jaeger span —
            # see the ordering note in planner.plan_node.
            trace_observation(
                f"agent.step.{step.kind.value}",
                input={
                    "step_id": step.id,
                    "kind": step.kind.value,
                    "name": step.tool,
                    "args": step.args,
                    "prompt": step.prompt,
                    "query": step.query,
                },
                metadata={
                    "step_id": step.id,
                    "kind": step.kind.value,
                    "tool": step.tool,
                },
            ) as lf_step,
            jaeger_span(tracer, f"agent.step.{step.kind.value}") as span,
        ):
            span.set_attribute("step_id", step.id)
            span.set_attribute("kind", step.kind.value)
            span.set_attribute("iteration", 0)
            if step.tool:
                span.set_attribute("name", step.tool)

            state["events_so_far"].append(
                Event(
                    kind="step_start",
                    payload=StepStartEvent(step_id=step.id, kind=step.kind).model_dump(),
                )
            )

            ok = True
            reason = ""
            try:
                if step.kind.value == "rag":
                    await _run_rag(state, step, rag, span)
                elif step.kind.value == "tool":
                    await _run_tool(
                        state,
                        step,
                        settings=settings,
                        tracer=tracer,
                        budget=budget,
                        tools=tools,
                        approvals=approvals,
                        span=span,
                    )
                elif step.kind.value == "llm":
                    await _run_llm(state, step, settings, tracer, budget, span)
                elif step.kind.value == "react":
                    await _run_react(state, step, settings, tracer, budget, tools, span, approvals=approvals)
                else:
                    raise ValueError(f"unsupported step kind: {step.kind}")
            except _BudgetExceededError:
                state["events_so_far"].append(
                    Event(
                        kind="step_end",
                        payload=StepEndEvent(step_id=step.id, status="failed", reason="budget_exceeded").model_dump(),
                    )
                )
                agent_steps_total.labels(step.kind.value, "budget_exceeded").inc()
                return _terminate_with_error(state, "budget_exceeded", "Per-user budget exhausted", settings)
            except _BudgetUnavailableError:
                state["events_so_far"].append(
                    Event(
                        kind="step_end",
                        payload=StepEndEvent(step_id=step.id, status="failed", reason="budget_unavailable").model_dump(),
                    )
                )
                agent_steps_total.labels(step.kind.value, "budget_unavailable").inc()
                return _terminate_with_error(state, "budget_unavailable", "Budget guard unavailable", settings)
            except Exception as exc:  # noqa: BLE001
                ok = False
                reason = repr(exc)
                span.record_exception(exc)

            state["events_so_far"].append(
                Event(
                    kind="step_end",
                    payload=StepEndEvent(step_id=step.id, status="ok" if ok else "failed", reason=reason or None).model_dump(),
                )
            )
            agent_steps_total.labels(step.kind.value, "ok" if ok else "failed").inc()
            update_observation(
                lf_step,
                output={"status": "ok" if ok else "failed", "reason": reason or None},
                level="ERROR" if not ok else "DEFAULT",
            )

    # Every step has run: surface a partial-failure note before the final
    # answer's deltas when the run diverged from the plan (spec:
    # agent-partial-failure). No-op on clean runs.
    _maybe_emit_partial_failure(state, settings)

    return state


# --- handlers ------------------------------------------------------------


async def _run_rag(state: AgentState, step, rag: RagClient, span: trace.Span) -> None:
    query = step.query or ""
    try:
        result = await rag.retrieve(query)
    except RagAuthError as exc:
        raise _step_runtime_error("RAG auth", repr(exc)) from exc
    except RagUnavailableError:
        # Per spec: empty context is acceptable; not a hard failure.
        state["step_outputs"][step.id] = {"context_block": "", "citations": []}
        span.set_attribute("rag_unavailable", True)
        return
    state["step_outputs"][step.id] = {"context_block": result.context_block, "citations": result.citations}


async def _run_tool(
    state: AgentState,
    step,
    *,
    settings: Settings,
    tracer: trace.Tracer,
    budget: BudgetGuard,
    tools: ToolDispatcher,
    approvals: ApprovalGate | None = None,
    span: trace.Span,
) -> None:
    if not step.tool:
        raise _step_runtime_error("tool", "step has no tool name")
    args = step.args or {}
    ok, result, error_text = await _dispatch_tool_with_retries(
        state,
        step_id=step.id,
        tool_name=step.tool,
        args=args,
        tools=tools,
        approvals=approvals,
        settings=settings,
        tracer=tracer,
        budget=budget,
        span=span,
    )
    if not ok:
        # `error_text` is the safe, funnel-mapped message — it is the only
        # text that reaches the wire (tool_result frame above) and the
        # step_end reason.
        raise _step_runtime_error("tool", error_text)
    state["step_outputs"][step.id] = result


async def _run_llm(state: AgentState, step, settings: Settings, tracer: trace.Tracer, budget: BudgetGuard, span: trace.Span) -> None:
    decision = await budget.check_and_increment(state["user_id"], DEFAULT_ESTIMATED_TOKENS, tracer)
    if decision.decision == "exceeded":
        raise _BudgetExceededError()
    if decision.decision == "unavailable":
        raise _BudgetUnavailableError()

    model = state.get("model") or settings.llm_model
    client = ChatOpenAI(
        base_url=settings.llm_base_url,
        api_key=settings.llm_api_key or "no-key",
        model=model,
        temperature=0.0,
    )

    msgs = [SystemMessage(content=step.prompt or "")]
    context = _render_prior_data(state, step.id)
    if context:
        msgs.append(SystemMessage(content=context))
    msgs.extend(_state_messages_to_lc(state.get("messages", [])))

    try:
        resp = await client.ainvoke(msgs)
    except Exception as exc:
        raise _step_runtime_error("llm", repr(exc)) from exc

    text = _extract_text(resp)
    state["step_outputs"][step.id] = text
    state["events_so_far"].append(Event(kind="delta", payload=DeltaEvent(delta=text).model_dump()))
    agent_tokens_total.labels("executor", model).inc(DEFAULT_ESTIMATED_TOKENS)
    state["cumulative_tokens"] += DEFAULT_ESTIMATED_TOKENS


async def _run_react(
    state: AgentState,
    step,
    settings: Settings,
    tracer: trace.Tracer,
    budget: BudgetGuard,
    tools: ToolDispatcher,
    span: trace.Span,
    approvals: ApprovalGate | None = None,
) -> None:
    max_iters = step.max_iterations or settings.step_react_max_iterations
    model = state.get("model") or settings.llm_model
    base_client = ChatOpenAI(
        base_url=settings.llm_base_url,
        api_key=settings.llm_api_key or "no-key",
        model=model,
        temperature=0.0,
    )

    # HIGH-7 fix: bind tool definitions from the registry so the model can emit
    # tool_calls. Without this, react is dead code — the model has no tools to call.
    try:
        tool_schemas = await tools.list_tools()
    except Exception as exc:  # noqa: BLE001
        span.set_attribute("react_tools_list_error", repr(exc))
        tool_schemas = []
    if tool_schemas:
        client = base_client.bind_tools(tool_schemas)
    else:
        client = base_client

    messages = [SystemMessage(content=step.prompt or "")]
    context = _render_prior_data(state, step.id)
    if context:
        messages.append(SystemMessage(content=context))
    messages.extend(_state_messages_to_lc(state.get("messages", [])))

    for i in range(max_iters):
        with jaeger_span(tracer, "agent.step.react") as iter_span:
            iter_span.set_attribute("step_id", step.id)
            iter_span.set_attribute("kind", "react")
            iter_span.set_attribute("iteration", i)

            decision = await budget.check_and_increment(state["user_id"], DEFAULT_ESTIMATED_TOKENS, tracer)
            if decision.decision == "exceeded":
                raise _BudgetExceededError()
            if decision.decision == "unavailable":
                raise _BudgetUnavailableError()

            try:
                resp = await client.ainvoke(messages)
            except Exception as exc:
                raise _step_runtime_error("react", repr(exc)) from exc

            tool_calls = _extract_tool_calls(resp)
            text = _extract_text(resp)
            if text:
                state["events_so_far"].append(Event(kind="delta", payload=DeltaEvent(delta=text).model_dump()))
            agent_tokens_total.labels("react", model).inc(DEFAULT_ESTIMATED_TOKENS)
            state["cumulative_tokens"] += DEFAULT_ESTIMATED_TOKENS

            if not tool_calls:
                # Final answer — done.
                state["step_outputs"][step.id] = text
                return

            messages.append(resp)
            for tc in tool_calls:
                tc_name = _tool_name(tc)
                tc_id = _tool_id(tc)
                args = _parse_args(_tool_arg(tc))
                # Route through the shared dispatcher: the retry budget is
                # per-(step, tool) and shared across this step's ReAct
                # iterations (design.md Open Questions, resolved 2026-10-13).
                _ok, _result, content = await _dispatch_tool_with_retries(
                    state,
                    step_id=step.id,
                    tool_name=tc_name,
                    args=args,
                    tools=tools,
                    approvals=approvals,
                    settings=settings,
                    tracer=tracer,
                    budget=budget,
                    span=span,
                    base_messages=messages,
                    step_prompt=step.prompt,
                    model_tool_call_id=tc_id,
                )
                messages.append(ToolMessage(content=content, tool_call_id=tc_id))

    # Cap hit — step failed.
    state["events_so_far"].append(
        Event(
            kind="error",
            payload=ErrorEvent(
                error="ReAct step exceeded iteration cap",
                code="react_iterations_exceeded",
                request_id=state.get("request_id"),
            ).model_dump(),
        )
    )
    raise _step_runtime_error("react", "react_max_iterations")


# --- helpers -------------------------------------------------------------


class _BudgetExceededError(Exception):
    pass


class _BudgetUnavailableError(Exception):
    pass


def _step_runtime_error(kind: str, msg: str) -> Exception:
    return RuntimeError(f"{kind}: {msg}")


def _state_messages_to_lc(messages) -> list:
    out = []
    for m in messages:
        role = m.role if hasattr(m, "role") else m.get("role", "user")
        content = m.content if hasattr(m, "content") else m.get("content", "")
        if role == "user":
            out.append(HumanMessage(content=content or ""))
        elif role == "assistant":
            out.append(AIMessage(content=content or ""))
        elif role == "system":
            out.append(SystemMessage(content=content or ""))
    return out


# Hard cap on the prior-data context block injected into the answering
# step's prompt. Picked for the small-model default (Qwen3-4B has a
# 4096-token context, and Chinese/Japanese tokenize at ~1 token per
# character — so the cap must be in characters, not tokens, and tight
# enough that the *whole* answer prompt (system + prior-data +
# conversation + answer) fits in ~3 000 characters under multilingual
# content). The exact number is best-effort: the goal is "small-model
# friendly by default, configurable for the few users running larger
# contexts". The OpenAIInvalidRequestError 400 the live agent hit on
# 2026-10-14 with Qwen3-4B + 5 web_search results is the
# motivating incident — those 5 results were ~5 000 framed characters
# which alone exceed 4 000 tokens of Chinese text.
PRIOR_DATA_MAX_CHARS = 2800
PRIOR_DATA_PER_SECTION_MAX_CHARS = 1500

PRIOR_DATA_TRUNCATION_MARKER = "\n[truncated: section exceeded {limit} characters]"


# Untrusted-content markers emitted by the tools on the wire
# (`<untrusted_web_content source="…">…</untrusted_web_content>`). The
# prior-data block feeds the SAME text back to the model for the answer
# step, and on a small model those markers consume 50-100% of the
# available context for the answer prompt. The model still needs the
# raw text to reason over; only the markers are stripped. The wire-
# side tool_result frame still carries the framed text — the framing
# is preserved end-to-end, just not doubled into the answer prompt.
_UNTRUSTED_OPEN_RE = re.compile(r"<untrusted_web_content[^>]*>")
_UNTRUSTED_CLOSE_RE = re.compile(r"</untrusted_web_content>")


def _cap_section(text: str, limit: int) -> str:
    if limit <= 0 or len(text) <= limit:
        return text
    return text[:limit] + PRIOR_DATA_TRUNCATION_MARKER.format(limit=limit)


def _strip_untrusted_markers(text: str) -> str:
    """Drop the wire-side untrusted-content framing from text destined
    for the *answer step's prompt*. The framing is still on the original
    tool_result frame; we only avoid re-pasting it into the prompt
    where it would consume most of a small model's context window.
    """
    text = _UNTRUSTED_OPEN_RE.sub("", text)
    text = _UNTRUSTED_CLOSE_RE.sub("", text)
    # Collapse the blank lines the stripped tags left behind, so the
    # answer prompt is easier on the model's eyes.
    return re.sub(r"\n{3,}", "\n\n", text)


def _strip_markers_recursive(value):
    """Apply `_strip_untrusted_markers` to every string leaf in a nested
    JSON-shaped value. Dicts and lists are walked; non-string scalars
    (numbers, bools, None) are returned unchanged.
    """
    if isinstance(value, str):
        return _strip_untrusted_markers(value)
    if isinstance(value, dict):
        return {k: _strip_markers_recursive(v) for k, v in value.items()}
    if isinstance(value, list):
        return [_strip_markers_recursive(v) for v in value]
    return value


def _render_prior_data(state: AgentState, current_step_id: str) -> str:
    """Render earlier data-step outputs (rag context / tool results) as a context block.

    Only `rag` and `tool` outputs are carried forward — they are the data
    producers. `llm`/`react` steps produce user-visible text (the deltas
    the client already renders); feeding them forward would only
    duplicate answers. Returns "" when no earlier data step produced
    anything, so data-free plans (e.g. a greeting) keep their exact
    prior prompt shape.

    The block is bounded by `PRIOR_DATA_MAX_CHARS` total (most recent
    step first; older sections are dropped when the cap is hit) and by
    `PRIOR_DATA_PER_SECTION_MAX_CHARS` per section (the per-section cap
    fires before the total so a single huge prior step cannot starve
    later ones). Bounding is what stops a 5-result `web_search` from
    pushing the answer-step prompt past a small model's context
    window (the 2026-10-14 in-the-wild repro: provider 400 on
    4097 input tokens).

    Motivating bug (2026-10, original): "summarize recent 3 months feed"
    planned a rag-only plan; the retrieved context was written to
    step_outputs and never read by anything, so the summary could never
    see the feeds.
    Motivating bug (2026-10-14, follow-up): web_search returning
    multiple results inflated the answer step's prompt past the
    small-model context window; provider rejected with
    OpenAIInvalidRequestError 400.
    """
    plan = state.get("plan")
    outputs = state.get("step_outputs") or {}
    if plan is None:
        return ""
    # Build sections in plan order first, then reverse so the most
    # recent data step wins when the total cap drops older sections.
    plan_sections: list[tuple[str, str]] = []
    for s in plan.steps:
        if s.id == current_step_id:
            break
        out = outputs.get(s.id)
        if out is None:
            continue
        if s.kind.value == "rag" and isinstance(out, dict):
            block = out.get("context_block")
            if block:
                # `rag` context blocks are bare text — strip markers
                # directly on the block.
                plan_sections.append(
                    (s.id, f"[{s.id} — retrieved context]\n{_strip_untrusted_markers(block)}")
                )
        elif s.kind.value == "tool":
            # `tool` sections JSON-dump the entire result dict, so the
            # `description` field carrying the untrusted-framed text is
            # *escaped* inside the JSON string. Walk the dict, strip
            # markers inside string fields, then re-emit. Anything
            # non-string (nested dict, list) is left as-is.
            stripped = _strip_markers_recursive(out)
            plan_sections.append(
                (
                    s.id,
                    f"[{s.id} — tool result from {s.tool}]\n{json.dumps(stripped, default=str)}",
                )
            )

    if not plan_sections:
        return ""

    # Per-section cap first (a single huge section must not eat the budget).
    # Strip the untrusted-content markers from each section's body before
    # capping — the markers are for the wire-side tool_result frame, not
    # the model's context. (See `_strip_untrusted_markers` for rationale.)
    capped = [
        (sid, _cap_section(_strip_untrusted_markers(body), PRIOR_DATA_PER_SECTION_MAX_CHARS))
        for sid, body in plan_sections
    ]
    # Total cap: keep the most recent section; if it alone fits, prepend
    # earlier ones while there is room.
    total = 0
    kept: list[str] = []
    for sid, body in reversed(capped):
        addition = len(body) + (2 if kept else 0)  # separator between sections
        if total + addition > PRIOR_DATA_MAX_CHARS:
            # First (newest) section never fits because of total cap alone:
            # cap it to whatever room remains. Earlier sections are dropped.
            if not kept:
                remaining = max(PRIOR_DATA_MAX_CHARS, 0)
                kept.append(_cap_section(body, remaining))
                total = PRIOR_DATA_MAX_CHARS
            continue
        kept.append(body)
        total += addition
    kept.reverse()

    return "Data gathered by earlier steps:\n\n" + "\n\n".join(kept)


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


def _extract_tool_calls(resp) -> list:
    if hasattr(resp, "tool_calls") and resp.tool_calls:
        return resp.tool_calls
    if isinstance(resp.content, list):
        out = []
        for block in resp.content:
            if isinstance(block, dict) and block.get("type") == "tool_use":
                out.append(block)
        return out
    return []


def _parse_args(raw) -> dict:
    if isinstance(raw, dict):
        return raw
    if isinstance(raw, str):
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            return {"_raw": raw}
    return {}


def _tool_arg(tc) -> str | dict:
    """Pull `args` from a tool call whether it's a LangChain ToolCall object or a dict.

    LangChain's AIMessage.tool_calls returns a list of `ToolCall` instances with `.id`/
    `.name`/`.args` attributes; some upstream wire shapes (and the test stub) put plain
    dicts in that list. Normalise to a dict.
    """
    if isinstance(tc, dict):
        return tc.get("args", {})
    return tc.args


def _tool_name(tc) -> str:
    if isinstance(tc, dict):
        return tc.get("name", "")
    return tc.name


def _tool_id(tc) -> str:
    if isinstance(tc, dict):
        return tc.get("id", "")
    return tc.id


def _terminate_with_error(state: AgentState, code: str, detail: str, settings: Settings) -> AgentState:
    state["terminal_status"] = code
    state["terminal_reason"] = code
    state["events_so_far"].append(
        Event(
            kind="error",
            payload=ErrorEvent(error=detail, code=code, request_id=state.get("request_id")).model_dump(),
        )
    )
    # Budget-driven termination is a plan divergence: surface the
    # partial-failure note (with the step that tripped the budget) before
    # the terminal frames.
    _maybe_emit_partial_failure(state, settings)
    state["events_so_far"].append(Event(kind="done", payload=DoneEvent().model_dump()))
    state["events_so_far"].append(
        Event(kind="terminator", payload=TerminatorEvent(reason=code).model_dump())
    )
    return state


# --- tool dispatch with retries ------------------------------------------


async def _dispatch_tool_with_retries(
    state: AgentState,
    *,
    step_id: str,
    tool_name: str,
    args: dict,
    tools: ToolDispatcher,
    approvals: ApprovalGate | None = None,
    settings: Settings,
    tracer: trace.Tracer,
    budget: BudgetGuard,
    span: trace.Span,
    base_messages: list | None = None,
    step_prompt: str | None = None,
    model_tool_call_id: str | None = None,
) -> tuple[bool, Any, str]:
    """Dispatch one tool call under the two retry paths.

    Mutating registry calls first pass the confirmation gate (D5): checked
    exactly once per (tool, args) pair, before that pair's first dispatch
    attempt. A denial returns `ok=False` with the model-visible denial
    text and consumes no retry budget.

    Returns `(ok, result, content)`:
      - `ok=True` → `result` is the raw tool result; `content` is its
        JSON rendering (model-facing).
      - `ok=False` → `result` is None; `content` is the safe error text
        (the same text already placed on the terminal `tool_result` frame).

    The two paths are mutually exclusive per failure (`_classify_tool_failure`):
      - registry rejection (`ToolError`) → tool-arg path: re-prompt the model
        with the validator's error so it emits a *corrected* call;
      - transient (`ToolUnavailableError`: connect/timeout/5xx/408/429) →
        retry the same call with jittered exponential backoff;
      - everything else (auth, bare 4xx, untyped) → fail fast, no retry.

    Budget: per-(step, tool) retry allowance of `settings.tool_retry_max`,
    persisted in `state["tool_retry_used"]` — shared across ReAct iterations
    within the step, fresh for each new step. A dispatch that starts with an
    exhausted allowance still attempts the call once, then fails fast.
    """
    used_by_tool = state.setdefault("tool_retry_used", {}).setdefault(step_id, {})
    used = used_by_tool.get(tool_name, 0)
    allowance = max(settings.tool_retry_max - used, 0)

    current_name = tool_name
    current_args = args
    retries_made = 0
    last_error_text = ""
    # Id of the call currently being dispatched. For the plain path the
    # first synthetic call gets a fresh id inside the re-prompt; after a
    # correction it is the model's own tool-call id, so each rejection's
    # ToolMessage answers the assistant turn that actually emitted it.
    current_call_id = model_tool_call_id
    # Prior re-prompt exchanges (rejection + feedback + the model's
    # corrected response, plus any synthetic assistant turn). Carried into
    # later re-prompts so retry >= 2 sees a fully paired message history
    # (test-review round 2, NEW-3): without this the first rejection sat
    # unanswered and two consecutive assistant tool-call turns reached
    # strict OpenAI-compatible providers, which 400 on that shape.
    reprompt_carry: list = []

    # Confirmation gate (openspec/changes/agent-feed-tools D5/D6): checked
    # before the first dispatch attempt of each new (tool, args) pair —
    # transient retries of an approved pair do NOT re-confirm (the approval
    # covers that intended mutation), while a corrected re-dispatch (new
    # args, or a corrected tool name, after a registry rejection) does: an
    # approval binds to the exact call it names.
    gated_key: tuple[str, str] | None = None

    while True:
        gate_key = (current_name, _canonical_args(current_args))
        if gate_key != gated_key:
            checker = getattr(tools, "confirmation_required", None)
            needs_confirmation = False
            if checker is not None:
                try:
                    needs_confirmation = await checker(current_name)
                except Exception as exc:  # noqa: BLE001 — fail toward confirmation
                    log.warning(
                        "confirmation_required failed; requiring confirmation: %r",
                        exc,
                    )
                    needs_confirmation = True
            if needs_confirmation:
                decision = DECISION_UNAVAILABLE
                if approvals is not None:
                    decision = await approvals.request_confirmation(
                        state,
                        step_id=step_id,
                        tool_name=current_name,
                        args=current_args,
                    )
                if decision != DECISION_APPROVE:
                    denial = denial_text_for(decision, current_name)
                    # Closed outcome, not a failure class: the tool is not
                    # dispatched, no retry-counter consumption, no re-prompt
                    # — the denial is a model-visible tool result (D6).
                    _emit_tool_result(
                        state,
                        step_id,
                        str(uuid.uuid4()),
                        ok=False,
                        error=denial,
                        retry_count=0,
                    )
                    return False, None, denial
            gated_key = gate_key
        call_id = str(uuid.uuid4())
        state["events_so_far"].append(
            Event(
                kind="tool_call",
                payload=ToolCallEvent(
                    step_id=step_id, call_id=call_id, name=current_name, args=current_args
                ).model_dump(),
            )
        )
        # Langfuse tool-call observation (agent-langfuse-tracing 3.4): one
        # span per dispatch attempt, carrying the masked args + outcome +
        # duration. The `with` wraps ONLY the call; classification below
        # stays outside it so an exception still reaches this frame's
        # handler with the observation already closed as ERROR.
        attempt_error: Exception | None = None
        with trace_observation(
            "agent.tool_call",
            input={"tool": current_name, "args": current_args},
            metadata={
                "tool": current_name,
                "step_id": step_id,
                "attempt": retries_made + 1,
            },
        ) as lf_tool:
            try:
                result = await tools.call(current_name, current_args)
            except Exception as exc:  # noqa: BLE001 — classified below, never re-raised raw
                attempt_error = exc
                update_observation(lf_tool, output={"status": "failed"}, level="ERROR")
            else:
                update_observation(lf_tool, output={"status": "ok"})

        if attempt_error is not None:
            exc = attempt_error
            failure_class = _classify_tool_failure(exc)
            last_error_text = _safe_tool_error_text(exc)
            agent_tool_calls_total.labels(current_name, "false").inc()
            # One failure frame per attempt. Intermediate retries carry the
            # retries made *so far* (0 for the first-attempt failure); the
            # terminal frame carries the same count at exhaustion — which
            # equals the allowance, i.e. `tool_retry_max` on a fresh budget.
            _emit_tool_result(
                state, step_id, call_id, ok=False, error=last_error_text, retry_count=retries_made
            )
            can_retry = failure_class != "non_retryable" and retries_made < allowance
            if not can_retry:
                used_by_tool[tool_name] = used + retries_made
                _record_terminal_tool_failure(state, step_id, tool_name)
                return False, None, last_error_text

            retries_made += 1
            reason = "tool_arg_invalid" if failure_class == "arg" else "tool_execution_transient"
            agent_tool_retries_total.labels(tool_name, reason).inc()
            span.set_attribute("retry_count", retries_made)
            span.set_attribute("retry_reason", reason)

            if failure_class == "transient":
                await asyncio.sleep(_compute_backoff_ms(retries_made - 1, settings) / 1000.0)
                continue

            # Tool-arg path: re-prompt the model for a corrected call. The
            # model saw the rejected call + validator error + schema; its
            # first tool call becomes the next attempt.
            corrected = await _reprompt_for_corrected_call(
                state,
                step_prompt=step_prompt,
                tool_name=current_name,
                args=current_args,
                error_text=last_error_text,
                base_messages=base_messages,
                model_tool_call_id=current_call_id,
                carry=reprompt_carry,
                settings=settings,
                tracer=tracer,
                budget=budget,
                tools=tools,
            )
            if corrected is None:
                used_by_tool[tool_name] = used + retries_made
                _record_terminal_tool_failure(state, step_id, tool_name)
                return (
                    False,
                    None,
                    (
                        f"model did not emit a corrected call for {tool_name} "
                        f"(last rejection: {last_error_text})"
                    ),
                )
            current_name, current_args, current_call_id, exchange = corrected
            reprompt_carry.extend(exchange)
            continue
        else:
            agent_tool_calls_total.labels(current_name, "true").inc()
            _emit_tool_result(
                state, step_id, call_id, ok=True, result=result, retry_count=retries_made
            )
            used_by_tool[tool_name] = used + retries_made
            return True, result, json.dumps(result, default=str)


async def _reprompt_for_corrected_call(
    state: AgentState,
    *,
    step_prompt: str | None,
    tool_name: str,
    args: dict,
    error_text: str,
    base_messages: list | None,
    model_tool_call_id: str | None,
    carry: list,
    settings: Settings,
    tracer: trace.Tracer,
    budget: BudgetGuard,
    tools: ToolDispatcher,
) -> tuple[str, dict, str, list] | None:
    """Re-invoke the model to correct a rejected tool call.

    Message-list shape (spec: `agent-tool-retry` » re-prompt structure):
    prior context, the assistant message that emitted the bad call, the
    matching tool result carrying the validator's error, and a final user
    message quoting the error + the tool's argument schema. `carry` holds
    the earlier re-prompt exchanges so each retry sees a fully paired
    history (no unanswered assistant turns).

    Returns `(corrected_name, corrected_args, corrected_call_id, exchange)`
    where `exchange` is the message block appended on top of base + carry —
    the caller extends `carry` with it so the NEXT retry stays paired. None
    when the model declines to emit any tool call. Raises the budget
    sentinels when the guard refuses the model call (a re-prompt *is* a
    model call — it must be gated like one).
    """
    decision = await budget.check_and_increment(state["user_id"], DEFAULT_ESTIMATED_TOKENS, tracer)
    if decision.decision == "exceeded":
        raise _BudgetExceededError()
    if decision.decision == "unavailable":
        raise _BudgetUnavailableError()

    model = state.get("model") or settings.llm_model
    base_client = ChatOpenAI(
        base_url=settings.llm_base_url,
        api_key=SecretStr(settings.llm_api_key or "no-key"),
        model=model,
        temperature=0.0,
    )
    try:
        schemas = await tools.list_tools()
    except Exception as exc:  # noqa: BLE001 — best-effort; the prompt still carries the error
        # Degraded, not fatal: the re-prompt proceeds without the argument
        # schema. Log so a registry outage during retries is visible.
        log.warning("re-prompt: tools/list failed; schema omitted from prompt: %r", exc)
        schemas = []
    if schemas:
        client = base_client.bind_tools(schemas)
    else:
        client = base_client

    tc_id = model_tool_call_id or str(uuid.uuid4())
    prompt_msgs: list = []
    if base_messages is not None:
        # ReAct: the live loop messages already end with the assistant
        # message that emitted the bad call.
        prompt_msgs.extend(base_messages)
    else:
        if step_prompt:
            prompt_msgs.append(SystemMessage(content=step_prompt))
        prompt_msgs.extend(_state_messages_to_lc(state.get("messages", [])))
    prompt_msgs.extend(carry)
    exchange: list = []
    # Append the synthetic assistant tool-call message ONLY when no message
    # in the list already carries this call id (test-review round 1, F2):
    # in the ReAct path the live messages (or a carried exchange) already
    # contain the assistant turn for this call, and appending a second copy
    # produces two consecutive assistant tool-call turns — which strict
    # OpenAI-compatible providers reject.
    def _carries_bad_call(msg) -> bool:
        for tc in getattr(msg, "tool_calls", None) or []:
            call_id = tc.get("id") if isinstance(tc, dict) else getattr(tc, "id", None)
            if call_id == tc_id:
                return True
        return False

    if not any(_carries_bad_call(m) for m in prompt_msgs):
        synthetic = AIMessage(
            content="", tool_calls=[{"id": tc_id, "name": tool_name, "args": args}]
        )
        prompt_msgs.append(synthetic)
        exchange.append(synthetic)
    rejection = ToolMessage(content=f"Tool call rejected: {error_text}", tool_call_id=tc_id)
    feedback = HumanMessage(
        content=(
            f"{PREVIOUS_ATTEMPT_FAILED_HEADER}: {error_text}\n\n"
            f"The `{tool_name}` tool expects arguments matching this schema:\n"
            f"{_tool_schema_text(schemas, tool_name)}\n\n"
            f"Emit a corrected tool call for `{tool_name}`."
        )
    )
    prompt_msgs.append(rejection)
    exchange.append(rejection)
    # Parallel-call invariant (review round 3): a ReAct turn may emit several
    # tool calls at once; answering only the rejected one leaves its siblings
    # unanswered and strict providers 400 the re-prompt. Placeholders answer
    # them (their real results land in the live loop right after).
    placeholders = _placeholder_tool_messages(prompt_msgs)
    prompt_msgs.extend(placeholders)
    exchange.extend(placeholders)
    prompt_msgs.append(feedback)
    exchange.append(feedback)

    try:
        resp = await client.ainvoke(prompt_msgs)
    except Exception as exc:
        # The provider failed during the re-prompt — a hard step failure,
        # not a retryable dispatch outcome. Class name only: no internal text.
        raise _step_runtime_error("tool", f"re-prompt failed ({type(exc).__name__})") from exc

    agent_tokens_total.labels("executor", model).inc(DEFAULT_ESTIMATED_TOKENS)
    state["cumulative_tokens"] += DEFAULT_ESTIMATED_TOKENS

    tool_calls = _extract_tool_calls(resp)
    if not tool_calls:
        return None
    tc = tool_calls[0]
    # The model's own response carries the corrected call's id (real
    # providers always set it); keep it so the next rejection's ToolMessage
    # answers this exact turn. Fall back to a fresh id for id-less stubs.
    corrected_id = _tool_id(tc) or str(uuid.uuid4())
    exchange.append(resp)
    return _tool_name(tc), _parse_args(_tool_arg(tc)), corrected_id, exchange


def _classify_tool_failure(exc: Exception) -> str:
    """Route a tool failure: "arg" (re-prompt) | "transient" (backoff) | "non_retryable".

    The classification lives here — the client only raises typed errors
    (spec: `agent-tool-retry` » classification lives in the executor).
    """
    if isinstance(exc, ToolError):
        # JSON-RPC error envelope — the registry rejected the call
        # semantically (validation, unknown tool). A corrected call can win.
        return "arg"
    if isinstance(exc, ToolUnavailableError):
        # Connect refused, timeout, 5xx, 408, 429 — the same call may win.
        return "transient"
    # ToolAuthError, ToolRejectedError, and anything untyped: retrying the
    # same input cannot succeed; re-prompting cannot fix auth.
    return "non_retryable"


def _safe_tool_error_text(exc: Exception) -> str:
    """Funnel tool errors to wire-safe text (never stack traces / internals).

    `ToolError`'s message is the registry's typed validator text — already
    mapped through the Go-side safe-error funnel (agent-tool-registry).
    The rest collapse to static strings; `ToolRejectedError` keeps only the
    HTTP status.
    """
    if isinstance(exc, ToolError):
        return str(exc) or "tool call rejected by the tool registry"
    if isinstance(exc, ToolAuthError):
        return "tool authentication failed"
    if isinstance(exc, ToolRejectedError):
        code = getattr(exc, "status_code", None)
        return f"tool call rejected (HTTP {code})" if code else "tool call rejected"
    if isinstance(exc, ToolUnavailableError):
        return "tool unavailable"
    return "tool call failed"


def _compute_backoff_ms(attempt: int, settings: Settings) -> float:
    """Jittered exponential backoff: min(base · 2^attempt · jitter, ceiling)."""
    raw = float(settings.tool_retry_backoff_base_ms) * (2**attempt)
    jittered = raw * (1.0 + random.uniform(-0.2, 0.2))
    return min(max(jittered, 0.0), float(settings.tool_retry_backoff_ceiling_ms))


def _canonical_args(args: dict) -> str:
    """Stable string key for an args dict — the approval-binding key (D5)."""
    try:
        return json.dumps(args, sort_keys=True, default=str)
    except (TypeError, ValueError):
        return repr(args)


def _tool_schema_text(schemas: list, tool_name: str) -> str:
    """Argument-schema JSON for `tool_name` from the normalized function
    shape the dispatcher serves (design D1/D8): `function.parameters`.
    Returns "{}" when the tool is unknown here or the schema is empty —
    the re-prompt still carries the rejection text."""
    for s in schemas:
        if not isinstance(s, dict):
            continue
        fn = s.get("function", s)
        if isinstance(fn, dict) and fn.get("name") == tool_name:
            parameters = fn.get("parameters")
            if isinstance(parameters, dict) and parameters:
                return json.dumps(parameters, default=str)
    return "{}"


def _placeholder_tool_messages(prompt_msgs: list) -> list:
    """Answer any still-unanswered tool calls in the most recent assistant
    turn that carries tool calls.

    Strict OpenAI-compatible providers reject a prompt where a tool call in
    an assistant turn has no matching tool response ("tool_call_ids did not
    have response messages"). The re-prompt for one rejected call of a
    parallel batch replays the whole turn, so its siblings need neutral
    placeholders; their real results land in the live loop immediately
    after this dispatch.
    """
    turn_ids: list[str] = []
    for msg in reversed(prompt_msgs):
        tcs = getattr(msg, "tool_calls", None)
        if isinstance(msg, AIMessage) and tcs:
            for tc in tcs:
                call_id = tc.get("id") if isinstance(tc, dict) else getattr(tc, "id", None)
                if isinstance(call_id, str) and call_id:
                    turn_ids.append(call_id)
            break
    if not turn_ids:
        return []
    answered = {m.tool_call_id for m in prompt_msgs if isinstance(m, ToolMessage)}
    return [
        ToolMessage(
            content="Tool call deferred: awaiting the sibling call's correction.",
            tool_call_id=call_id,
        )
        for call_id in turn_ids
        if call_id not in answered
    ]


def _record_terminal_tool_failure(state: AgentState, step_id: str, tool_name: str) -> None:
    """Mark a (step, tool) whose dispatch ended in a TERMINAL failure.

    Recorded at the moment the dispatch gives up — exhaustion, a
    non-retryable class, or the model declining to emit a corrected call.
    Deliberately NOT derived from `tool_retry_used`: consuming the full
    allowance and then RECOVERING on the last allowed attempt is not a
    terminal failure and must not flag the step (test-review round 2,
    NEW-1 — the over-broad allowance check flagged green runs).
    """
    by_step = state.setdefault("tool_terminal_failures", {}).setdefault(step_id, [])
    if tool_name not in by_step:
        by_step.append(tool_name)


def _emit_tool_result(
    state: AgentState,
    step_id: str,
    call_id: str,
    *,
    ok: bool,
    result: Any | None = None,
    error: str | None = None,
    retry_count: int = 0,
) -> None:
    state["events_so_far"].append(
        Event(
            kind="tool_result",
            payload=ToolResultEvent(
                step_id=step_id,
                call_id=call_id,
                ok=ok,
                result=result,
                error=error,
                retry_count=retry_count,
            ).model_dump(),
        )
    )


# --- partial-failure signal (spec: agent-partial-failure) -----------------


def _collect_failed_steps(state: AgentState, settings: Settings) -> list[str]:
    """Step ids whose terminal status diverged from the plan, in first-signal order.

    Single source of truth for the banner trigger (spec scenario: "one
    function that the SSE publisher calls"):
      - any `step_end` with `status == "failed"`;
      - any `tool_result{ok: false}` for a (step, tool) that either consumed
        the full retry allowance (`retry_count == retry_max` — fallback for
        states replayed from checkpoints predating the terminal marker) or
        was recorded as a TERMINAL failure (`state["tool_terminal_failures"]`
        — exhaustion, non-retryable, or model-declined; covers allowances
        split across ReAct iterations). A dispatch that recovered on a later
        attempt records nothing and its success frame is excluded by the
        `ok is False` check.
    RAG-empty results reach `ok: true` and are excluded by construction.
    """
    events = state.get("events_so_far", [])
    retry_max = settings.tool_retry_max
    # Steps with a recorded TERMINAL dispatch failure
    # (`_record_terminal_tool_failure`). Covers the cases where no single
    # frame carries `retry_count == retry_max`: an allowance split across
    # ReAct iterations, the fail-fast dispatch after exhaustion, and
    # non-retryable (auth / bare-4xx) rejections. A dispatch that recovered
    # on its last allowed attempt records nothing here and its success
    # frame's `ok is True` excludes it from the second clause too
    # (test-review rounds 1+2, F3/NEW-1).
    terminal_failures = state.get("tool_terminal_failures") or {}
    seen: dict[str, None] = {}
    for ev in events:
        payload = ev.payload or {}
        is_failed_step = ev.kind == "step_end" and payload.get("status") == "failed"
        is_terminal_tool_failure = (
            ev.kind == "tool_result"
            and payload.get("ok") is False
            and (
                payload.get("retry_count", 0) == retry_max
                or payload.get("step_id") in terminal_failures
            )
        )
        if is_failed_step or is_terminal_tool_failure:
            step_id = payload.get("step_id")
            if step_id:
                seen.setdefault(step_id, None)
    return list(seen)


def _has_plan_level_error(state: AgentState) -> bool:
    for ev in state.get("events_so_far", []):
        if ev.kind == "error" and (ev.payload or {}).get("code") in PLAN_LEVEL_ERROR_CODES:
            return True
    return False


def _note_insert_index(events: list) -> int:
    """Position for the partial-failure note: before the final answer's deltas.

    The final answer's deltas are the last contiguous run of `delta` events.
    With no deltas (plan-level errors), the note goes before the first
    `done` frame if one is already present (the planner emits done itself on
    its terminal errors), else at the end (the graph appends done after the
    executor returns).
    """
    last_delta = None
    for i in range(len(events) - 1, -1, -1):
        if events[i].kind == "delta":
            last_delta = i
            break
    if last_delta is None:
        for i, ev in enumerate(events):
            if ev.kind == "done":
                return i
        return len(events)
    start = last_delta
    while start > 0 and events[start - 1].kind == "delta":
        start -= 1
    return start


def _maybe_emit_partial_failure(state: AgentState, settings: Settings) -> None:
    """Emit `note{kind: "partial_failure"}` when the run diverged from the plan.

    Trigger: any collected failed step OR a plan-level error code. No-op
    otherwise (clean runs, `run_metadata_unavailable`-only runs, RAG-empty).
    Idempotent — a run that already carries a partial-failure note (e.g.
    the planner emitted one for a plan-level error) is left untouched.
    """
    events = state["events_so_far"]
    if any(
        e.kind == "note" and (e.payload or {}).get("kind") == "partial_failure"
        for e in events
    ):
        return
    failed_steps = _collect_failed_steps(state, settings)
    if not failed_steps and not _has_plan_level_error(state):
        return
    events.insert(
        _note_insert_index(events),
        Event(
            kind="note",
            payload=NoteEvent(
                text="Run diverged from the plan",
                kind="partial_failure",
                failed_steps=failed_steps,
            ).model_dump(),
        ),
    )