"""LangGraph state graph wiring.

Replaces the hand-rolled `await plan_node; await executor_node` pipeline with a real
`langgraph.graph.StateGraph`. The graph is:

    START → planner → executor → END

Each node appends typed events to `state["events_so_far"]`. The SSE emitter in `main.py`
walks the events after each node yields (via `graph.astream`) so the client sees frames
incrementally — a slow step doesn't block the user from seeing earlier ones.

When a checkpointer is passed (langGraph `AsyncPostgresSaver` for production),
every node transition is automatically checkpointed under
`config={"configurable":{"thread_id": <run_id>}}` and resume is supported via
`graph.get_state(config)`. The checkpointer is optional; in tests we still
want the in-memory version (faster, no Postgres dependency).
"""

from __future__ import annotations

from functools import partial
from typing import Any

from langgraph.graph import END, START, StateGraph

from .budget import BudgetGuard
from .config import Settings
from .executor import executor_node
from .observability import agent_runs_total
from .planner import plan_node
from .rag_client import RagClient
from .state import AgentState
from .tools_client import ToolsClient
from .types import DoneEvent, DoneUsage, ErrorEvent, Event, TerminatorEvent


def _executor_step_factory(
    settings: Settings,
    budget: BudgetGuard,
    rag: RagClient,
    tools: ToolsClient,
):
    """Closure that returns the LangGraph-compatible async node for the executor."""
    async def _executor_step(state: AgentState) -> AgentState:
        from opentelemetry import trace

        tracer = trace.get_tracer("nyx.agent")
        return await executor_node(
            state, settings=settings, tracer=tracer, budget=budget, rag=rag, tools=tools
        )

    return _executor_step


def _planner_step_factory(settings: Settings):
    """Closure that returns the LangGraph-compatible async node for the planner."""
    async def _planner_step(state: AgentState) -> AgentState:
        from opentelemetry import trace

        tracer = trace.get_tracer("nyx.agent")
        return await plan_node(state, settings, tracer)

    return _planner_step


def build_graph(
    settings: Settings,
    budget: BudgetGuard,
    rag: RagClient,
    tools: ToolsClient,
    checkpointer: Any | None = None,
):
    """Build (and return) the compiled StateGraph ready for `astream(initial_state)`.

    When `checkpointer` is provided (langGraph `AsyncPostgresSaver` in production),
    per-step persistence is automatic; resume is supported via
    `graph.get_state({"configurable":{"thread_id": run_id}})`. In tests
    `checkpointer` is left as None and the graph runs purely in memory.
    """
    builder = StateGraph(AgentState)
    builder.add_node("planner", _planner_step_factory(settings))
    builder.add_node("executor", _executor_step_factory(settings, budget, rag, tools))
    builder.add_edge(START, "planner")
    builder.add_edge("planner", "executor")
    builder.add_edge("executor", END)
    if checkpointer is not None:
        return builder.compile(checkpointer=checkpointer)
    return builder.compile()


async def run_agent(
    state: AgentState,
    *,
    settings: Settings,
    tracer,  # noqa: ARG001 — kept for signature parity with the prior implementation
    budget: BudgetGuard,
    rag: RagClient,
    tools: ToolsClient,
) -> AgentState:
    """Run the compiled graph over the given initial state.

    Awaits the full graph (planner → executor → END). The internal `astream` call is
    used so we can emit per-node metric increments for observability — the actual
    SSE streaming happens upstream in `main.py` by calling `run_agent_stream` instead,
    which yields events as the graph progresses.
    """
    with tracer.start_as_current_span("agent.run") as span:
        span.set_attribute("run_id", state["run_id"])
        span.set_attribute("user_id", state["user_id"])

        graph = build_graph(settings, budget, rag, tools)
        # `ainvoke` returns the final state. Use the streaming variant below
        # in `run_agent_stream` for incremental frames.
        final = await graph.ainvoke(state)

        # Render terminal events if the planner/executor didn't already.
        status = final.get("terminal_status", "ok")
        if status == "ok":
            usage = DoneUsage(
                prompt_tokens=0,
                completion_tokens=final.get("cumulative_tokens", 0),
                total_tokens=final.get("cumulative_tokens", 0),
            )
            final["events_so_far"].append(Event(kind="done", payload=DoneEvent(usage=usage).model_dump()))
            final["events_so_far"].append(
                Event(kind="terminator", payload=TerminatorEvent(reason=None).model_dump())
            )
            agent_runs_total.labels("executor", "ok").inc()
        elif status == "failed":
            agent_runs_total.labels("executor", "failed").inc()
        else:
            agent_runs_total.labels("executor", status).inc()

        plan = final.get("plan")
        plan_size = len(plan.steps) if plan else 0
        span.set_attribute("plan_size", plan_size)
        span.set_attribute("total_tokens", final.get("cumulative_tokens", 0))
        return final


async def run_agent_stream(
    state: AgentState,
    *,
    settings: Settings,
    tracer,
    budget: BudgetGuard,
    rag: RagClient,
    tools: ToolsClient,
    graph=None,
    graph_config: dict | None = None,
    resumed: bool = False,
):
    """Async iterator: yields newly-appended events from `state["events_so_far"]`
    as the graph progresses. Yields one `AgentState` snapshot per node completion,
    plus a final snapshot after terminal-event emission.

    `graph` is pre-built (the caller may have wired a checkpointer). When
    `graph_config` is provided (typical when a checkpointer is in use),
    `astream` resumes from the latest checkpoint for that `thread_id` —
    so completed nodes are not re-executed. `resumed=True` skips the
    initial `plan` re-emission if the plan is already in the resumed state.
    """
    if graph is None:
        graph = build_graph(settings, budget, rag, tools)

    last_len = len(state.get("events_so_far", []))

    astream_kwargs: dict = {"stream_mode": "values"}
    if graph_config is not None:
        astream_kwargs["config"] = graph_config

    async for chunk in graph.astream(state, **astream_kwargs):
        # `chunk` is the post-node state snapshot.
        current_events = chunk.get("events_so_far", [])
        for ev in current_events[last_len:]:
            yield ev
        last_len = len(current_events)

    # After the graph finishes, render terminal frames if not already present.
    final = chunk if "chunk" in locals() else state  # last seen
    status = final.get("terminal_status", "ok")
    if status == "ok":
        # Guard: a plan that gathered data but never produced an answer
        # (e.g. a retrieval-only plan) must not finish silently green —
        # the user would see steps but no text, and the run row would
        # read `completed`. Emit a typed error; main.py's SSE consumer
        # marks the run failed on any error frame.
        events = final.get("events_so_far", [])
        produced_text = any(
            e.kind == "delta" and (e.payload or {}).get("delta") for e in events
        )
        if not produced_text:
            yield Event(
                kind="error",
                payload=ErrorEvent(
                    error="Agent produced no answer",
                    code="empty_answer",
                    request_id=final.get("request_id"),
                ).model_dump(),
            )
        usage = DoneUsage(
            prompt_tokens=0,
            completion_tokens=final.get("cumulative_tokens", 0),
            total_tokens=final.get("cumulative_tokens", 0),
        )
        new_events = [
            Event(kind="done", payload=DoneEvent(usage=usage).model_dump()),
            Event(kind="terminator", payload=TerminatorEvent(reason=None).model_dump()),
        ]
        for ev in new_events:
            yield ev
        agent_runs_total.labels("executor", "ok").inc()