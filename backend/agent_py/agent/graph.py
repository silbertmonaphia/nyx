"""LangGraph state graph wiring.

Replaces the hand-rolled `await plan_node; await executor_node` pipeline with a real
`langgraph.graph.StateGraph`. The graph is:

    START → planner → executor → END

Each node appends typed events to `state["events_so_far"]`. The SSE emitter in `main.py`
walks the events after each node yields (via `graph.astream`) so the client sees frames
incrementally — a slow step doesn't block the user from seeing earlier ones.
"""

from __future__ import annotations

from functools import partial

from langgraph.graph import END, START, StateGraph

from .budget import BudgetGuard
from .config import Settings
from .executor import executor_node
from .observability import agent_runs_total
from .planner import plan_node
from .rag_client import RagClient
from .state import AgentState
from .tools_client import ToolsClient
from .types import DoneEvent, DoneUsage, Event, TerminatorEvent


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
):
    """Build (and return) the compiled StateGraph ready for `astream(initial_state)`."""
    builder = StateGraph(AgentState)
    builder.add_node("planner", _planner_step_factory(settings))
    builder.add_node("executor", _executor_step_factory(settings, budget, rag, tools))
    builder.add_edge(START, "planner")
    builder.add_edge("planner", "executor")
    builder.add_edge("executor", END)
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
):
    """Async iterator: yields newly-appended events from `state["events_so_far"]`
    as the graph progresses. Yields one `AgentState` snapshot per node completion,
    plus a final snapshot after terminal-event emission.
    """
    graph = build_graph(settings, budget, rag, tools)
    last_len = len(state.get("events_so_far", []))

    async for chunk in graph.astream(state, stream_mode="values"):
        # `chunk` is the post-node state snapshot.
        current_events = chunk.get("events_so_far", [])
        for ev in current_events[last_len:]:
            yield ev
        last_len = len(current_events)

    # After the graph finishes, render terminal frames if not already present.
    final = chunk if "chunk" in locals() else state  # last seen
    status = final.get("terminal_status", "ok")
    if status == "ok":
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