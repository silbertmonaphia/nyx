"""Executor node — sequential over Plan.steps.

For each step runs the appropriate handler:
  - rag   → RagClient.retrieve → render context block into state.step_outputs[step.id]
  - tool  → ToolsClient.call   → render result
  - llm   → direct LLM call with state.messages + step.prompt (+ data from earlier rag/tool steps) → accumulate text deltas
  - react → nested subgraph (model → tool → model), capped by max_iterations

The executor is budget-aware: before every model call it consults BudgetGuard. Budget
exhaustion emits `error{code:"budget_exceeded"}` + done + terminator.
"""

from __future__ import annotations

import json
import uuid

from langchain_core.messages import AIMessage, HumanMessage, SystemMessage, ToolMessage
from langchain_openai import ChatOpenAI
from opentelemetry import trace

from .budget import BudgetGuard
from .config import Settings
from .observability import agent_steps_total, agent_tokens_total, agent_tool_calls_total
from .rag_client import RagAuthError, RagClient, RagUnavailableError
from .state import AgentState
from .tools_client import ToolAuthError, ToolError, ToolUnavailableError, ToolsClient
from .types import (
    DeltaEvent,
    DoneEvent,
    ErrorEvent,
    Event,
    Plan,
    StepEndEvent,
    StepStartEvent,
    TerminatorEvent,
    ToolCallEvent,
    ToolResultEvent,
)

DEFAULT_ESTIMATED_TOKENS = 1000


async def executor_node(
    state: AgentState,
    *,
    settings: Settings,
    tracer: trace.Tracer,
    budget: BudgetGuard,
    rag: RagClient,
    tools: ToolsClient,
) -> AgentState:
    plan: Plan | None = state.get("plan")
    if plan is None:
        state["terminal_status"] = "failed"
        state["terminal_reason"] = "no_plan"
        return state

    for step in plan.steps:
        with tracer.start_as_current_span(f"agent.step.{step.kind.value}") as span:
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
                    await _run_tool(state, step, tools, span)
                elif step.kind.value == "llm":
                    await _run_llm(state, step, settings, tracer, budget, span)
                elif step.kind.value == "react":
                    await _run_react(state, step, settings, tracer, budget, tools, span)
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
                return _terminate_with_error(state, "budget_exceeded", "Per-user budget exhausted")
            except _BudgetUnavailableError:
                state["events_so_far"].append(
                    Event(
                        kind="step_end",
                        payload=StepEndEvent(step_id=step.id, status="failed", reason="budget_unavailable").model_dump(),
                    )
                )
                agent_steps_total.labels(step.kind.value, "budget_unavailable").inc()
                return _terminate_with_error(state, "budget_unavailable", "Budget guard unavailable")
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

    return state


# --- handlers ------------------------------------------------------------


async def _run_rag(state: AgentState, step, rag: RagClient, span: trace.Span) -> None:
    query = step.query or ""
    try:
        result = await rag.retrieve(query)
    except RagAuthError as exc:
        raise _step_runtime_error("RAG auth", repr(exc)) from exc
    except RagUnavailableError as exc:
        # Per spec: empty context is acceptable; not a hard failure.
        state["step_outputs"][step.id] = {"context_block": "", "citations": []}
        span.set_attribute("rag_unavailable", True)
        return
    state["step_outputs"][step.id] = {"context_block": result.context_block, "citations": result.citations}


async def _run_tool(state: AgentState, step, tools: ToolsClient, span: trace.Span) -> None:
    if not step.tool:
        raise _step_runtime_error("tool", "step has no tool name")
    args = step.args or {}
    call_id = str(uuid.uuid4())
    state["events_so_far"].append(
        Event(
            kind="tool_call",
            payload=ToolCallEvent(step_id=step.id, call_id=call_id, name=step.tool, args=args).model_dump(),
        )
    )
    try:
        result = await tools.call(step.tool, args)
        agent_tool_calls_total.labels(step.tool, "true").inc()
        state["events_so_far"].append(
            Event(
                kind="tool_result",
                payload=ToolResultEvent(step_id=step.id, call_id=call_id, ok=True, result=result).model_dump(),
            )
        )
        state["step_outputs"][step.id] = result
    except (ToolAuthError, ToolError) as exc:
        agent_tool_calls_total.labels(step.tool, "false").inc()
        state["events_so_far"].append(
            Event(
                kind="tool_result",
                payload=ToolResultEvent(step_id=step.id, call_id=call_id, ok=False, error=repr(exc)).model_dump(),
            )
        )
        raise _step_runtime_error("tool", repr(exc)) from exc
    except ToolUnavailableError as exc:
        agent_tool_calls_total.labels(step.tool, "false").inc()
        state["events_so_far"].append(
            Event(
                kind="tool_result",
                payload=ToolResultEvent(step_id=step.id, call_id=call_id, ok=False, error="tool unavailable").model_dump(),
            )
        )
        raise _step_runtime_error("tool", "tool unavailable") from exc


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
    except Exception as exc:  # noqa: BLE001
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
    tools: ToolsClient,
    span: trace.Span,
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
        with tracer.start_as_current_span("agent.step.react") as iter_span:
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
            except Exception as exc:  # noqa: BLE001
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
                call_id = str(uuid.uuid4())
                tc_name = _tool_name(tc)
                tc_id = _tool_id(tc)
                args = _parse_args(_tool_arg(tc))
                state["events_so_far"].append(
                    Event(
                        kind="tool_call",
                        payload=ToolCallEvent(step_id=step.id, call_id=call_id, name=tc_name, args=args).model_dump(),
                    )
                )
                try:
                    result = await tools.call(tc_name, args)
                    agent_tool_calls_total.labels(tc_name, "true").inc()
                    state["events_so_far"].append(
                        Event(
                            kind="tool_result",
                            payload=ToolResultEvent(step_id=step.id, call_id=call_id, ok=True, result=result).model_dump(),
                        )
                    )
                    tool_msg = ToolMessage(content=json.dumps(result), tool_call_id=tc_id)
                except Exception as exc:  # noqa: BLE001
                    agent_tool_calls_total.labels(tc_name, "false").inc()
                    state["events_so_far"].append(
                        Event(
                            kind="tool_result",
                            payload=ToolResultEvent(step_id=step.id, call_id=call_id, ok=False, error=repr(exc)).model_dump(),
                        )
                    )
                    tool_msg = ToolMessage(content=repr(exc), tool_call_id=tc_id)
                messages.append(tool_msg)

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


def _render_prior_data(state: AgentState, current_step_id: str) -> str:
    """Render earlier data-step outputs (rag context / tool results) as a context block.

    Only `rag` and `tool` outputs are carried forward — they are the data
    producers. `llm`/`react` steps produce user-visible text (the deltas
    the client already renders); feeding them forward would only
    duplicate answers. Returns "" when no earlier data step produced
    anything, so data-free plans (e.g. a greeting) keep their exact
    prior prompt shape.

    Motivating bug (2026-10): "summarize recent 3 months feed" planned a
    rag-only plan; the retrieved context was written to step_outputs and
    never read by anything, so the summary could never see the feeds.
    """
    plan = state.get("plan")
    outputs = state.get("step_outputs") or {}
    if plan is None:
        return ""
    sections: list[str] = []
    for s in plan.steps:
        if s.id == current_step_id:
            break
        out = outputs.get(s.id)
        if out is None:
            continue
        if s.kind.value == "rag" and isinstance(out, dict):
            block = out.get("context_block")
            if block:
                sections.append(f"[{s.id} — retrieved context]\n{block}")
        elif s.kind.value == "tool":
            sections.append(
                f"[{s.id} — tool result from {s.tool}]\n{json.dumps(out, default=str)}"
            )
    if not sections:
        return ""
    return "Data gathered by earlier steps:\n\n" + "\n\n".join(sections)


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


def _terminate_with_error(state: AgentState, code: str, detail: str) -> AgentState:
    state["terminal_status"] = code
    state["terminal_reason"] = code
    state["events_so_far"].append(
        Event(
            kind="error",
            payload=ErrorEvent(error=detail, code=code, request_id=state.get("request_id")).model_dump(),
        )
    )
    state["events_so_far"].append(Event(kind="done", payload=DoneEvent().model_dump()))
    state["events_so_far"].append(
        Event(kind="terminator", payload=TerminatorEvent(reason=code).model_dump())
    )
    return state