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

from typing import Any

from langgraph.graph import END, START, StateGraph

from .approvals import ApprovalGate, RunSink
from .budget import BudgetGuard
from .config import Settings
from .executor import executor_node
from .observability import agent_runs_total
from .planner import plan_node
from .rag_client import RagClient
from .state import AgentState
from .tool_dispatch import ToolDispatcher
from .types import DoneEvent, DoneUsage, ErrorEvent, Event, TerminatorEvent


def _executor_step_factory(
    settings: Settings,
    budget: BudgetGuard,
    rag: RagClient,
    tools: ToolDispatcher,
    tracer: Any,
    approvals: ApprovalGate | None = None,
):
    """Closure that returns the LangGraph-compatible async node for the executor.

    `approvals` (openspec/changes/agent-feed-tools D5) gates mutating
    registry calls at the shared dispatch funnel; `None` leaves the gate
    fail-closed (no gate configured ⇒ mutating calls close as denied).
    """
    async def _executor_step(state: AgentState) -> AgentState:
        return await executor_node(
            state,
            settings=settings,
            tracer=tracer,
            budget=budget,
            rag=rag,
            tools=tools,
            approvals=approvals,
        )

    return _executor_step


def _planner_step_factory(
    settings: Settings,
    tracer: Any,
    prefs_text: str = "",
    tools: ToolDispatcher | None = None,
):
    """Closure that returns the LangGraph-compatible async node for the planner.

    `prefs_text` is the rendered per-user preferences block
    (openspec/changes/agent-user-preferences D5) the caller loaded before
    building the graph; it rides on the node closure into
    `plan_node` → `_initial_messages`. `tools` (agent-feed-tools D4) rides
    the same way so the planner can advertise the live registry catalog —
    mirroring the executor factory, which already receives the dispatcher.
    """
    async def _planner_step(state: AgentState) -> AgentState:
        return await plan_node(
            state, settings, tracer, prefs_text=prefs_text, tools=tools
        )

    return _planner_step


def build_graph(
    settings: Settings,
    budget: BudgetGuard,
    rag: RagClient,
    tools: ToolDispatcher,
    checkpointer: Any | None = None,
    tracer: Any | None = None,
    prefs_text: str = "",
    approvals: ApprovalGate | None = None,
):
    """Build (and return) the compiled StateGraph ready for `astream(initial_state)`.

    When `checkpointer` is provided (langGraph `AsyncPostgresSaver` in production),
    per-step persistence is automatic; resume is supported via
    `graph.get_state({"configurable":{"thread_id": run_id}})`. In tests
    `checkpointer` is left as None and the graph runs purely in memory.

    `tracer` is the OTel tracer the nodes emit their spans on. It should be
    the same tracer instance the service was started with (`app.state.tracer`
    in main.py) — the nodes previously resolved `trace.get_tracer(...)` from
    the GLOBAL provider, which silently diverged from the service's tracer
    (found while building the enabled-vs-disabled Jaeger tree regression
    test: a second app in the same process exports its node spans to the
    first app's provider). Falls back to the global lookup when omitted, for
    callers that don't care.

    `prefs_text` is the per-user preferences block (openspec/changes/
    agent-user-preferences D5) the caller loaded from the PrefsStore before
    building the graph. The default `""` keeps every pre-existing caller's
    planner prompt byte-identical.
    """
    if tracer is None:
        from opentelemetry import trace

        tracer = trace.get_tracer("nyx.agent")
    builder = StateGraph(AgentState)
    builder.add_node(
        "planner", _planner_step_factory(settings, tracer, prefs_text, tools)
    )
    builder.add_node(
        "executor",
        _executor_step_factory(settings, budget, rag, tools, tracer, approvals),
    )
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
    tracer,
    budget: BudgetGuard,
    rag: RagClient,
    tools: ToolDispatcher,
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

        graph = build_graph(settings, budget, rag, tools, tracer=tracer)
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
    tools: ToolDispatcher,
    graph=None,
    graph_config: dict | None = None,
    resumed: bool = False,
    sink: RunSink | None = None,
):
    """Async iterator: yields newly-appended events from `state["events_so_far"]`
    as the graph progresses. Yields one `AgentState` snapshot per node completion,
    plus a final snapshot after terminal-event emission.

    `graph` is pre-built (the caller may have wired a checkpointer). When
    `graph_config` is provided (typical when a checkpointer is in use),
    `astream` resumes from the latest checkpoint for that `thread_id` —
    so completed nodes are not re-executed. `resumed=True` skips the
    initial `plan` re-emission if the plan is already in the resumed state.

    `sink` (optional) switches delivery from per-node yields to the live
    RunSink channel (agent-feed-tools D2 note): frames flush as they
    appear — including the gate's mid-node confirm prompt — and nothing is
    yielded; the caller drains the sink's queue. Without a sink the legacy
    per-node yield behavior is preserved (unit tests, `run_agent`).
    """
    if graph is None:
        graph = build_graph(settings, budget, rag, tools, tracer=tracer)

    last_len = len(state.get("events_so_far", []))

    astream_kwargs: dict = {"stream_mode": "values"}
    if graph_config is not None:
        astream_kwargs["config"] = graph_config

    # Resume-safety (found 2026-10-14 while landing agent-stale-run-sweeper):
    # langgraph treats a non-None `astream` input as a state UPDATE — passing
    # the freshly-built `new_state()` on resume CLOBBERS every checkpointed
    # channel (plan, step_outputs, events_so_far …), so the planner and the
    # already-completed steps re-ran (token + tool double-spend). Even an
    # empty dict `{}` schedules from the entrypoint (verified: the planner
    # re-ran); the correct pure-resume idiom is `None`, which continues from
    # the checkpoint's pending nodes — and when the graph already reached
    # END, yields the final snapshot without raising. Fall back to the fresh
    # state only when no checkpoint exists yet (the process died before the
    # first node finished).
    stream_input: AgentState | dict | None = state
    if resumed and graph_config is not None:
        snapshot = await graph.aget_state(graph_config)
        if snapshot is not None and snapshot.values:
            stream_input = None

    async for chunk in graph.astream(stream_input, **astream_kwargs):
        # `chunk` is the post-node state snapshot.
        current_events = chunk.get("events_so_far", [])
        if sink is not None:
            # Live mode: the gate already flushed mid-node frames; this
            # boundary flush catches everything else (identity-based, so
            # nothing is delivered twice).
            await sink.flush(current_events)
        else:
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
        final_events = final.get("events_so_far", [])
        produced_text = any(
            e.kind == "delta" and (e.payload or {}).get("delta") for e in final_events
        )
        new_events: list = []
        if not produced_text:
            new_events.append(
                Event(
                    kind="error",
                    payload=ErrorEvent(
                        error="Agent produced no answer",
                        code="empty_answer",
                        request_id=final.get("request_id"),
                    ).model_dump(),
                )
            )
        usage = DoneUsage(
            prompt_tokens=0,
            completion_tokens=final.get("cumulative_tokens", 0),
            total_tokens=final.get("cumulative_tokens", 0),
        )
        new_events.append(Event(kind="done", payload=DoneEvent(usage=usage).model_dump()))
        new_events.append(
            Event(kind="terminator", payload=TerminatorEvent(reason=None).model_dump())
        )
        if sink is not None:
            # Live mode: terminal frames ride the state list too, so the
            # single delivery path ships them exactly once.
            final_events.extend(new_events)
            await sink.flush(final_events)
        else:
            for ev in new_events:
                yield ev
        agent_runs_total.labels("executor", "ok").inc()