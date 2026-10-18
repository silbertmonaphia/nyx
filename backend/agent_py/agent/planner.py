"""Planner node — emit a Pydantic `Plan` for the executor.

Primary path (default): the planner binds the OpenAI `response_format`
json_schema (explicitly, with `strict: false` — see
`_build_structured_client` for why `with_structured_output` is not used),
so the provider supplies a schema-conformant JSON object directly — no
`json.loads` on free-text model output. A bounded retry loop catches
`pydantic.ValidationError` / `langchain_core.exceptions.OutputParserException`,
appends the validator's last error to the re-prompt under a
`PREVIOUS_ATTEMPT_FAILED` block, and re-invokes up to
`PYTHON_AGENT_PLANNER_RETRY_MAX` (default 1) before terminating the run
with `error{code:"plan_invalid"}`.

Legacy fallback (`PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false`): the
free-text path stays exactly as it was — fence-strip then `json.loads`
then per-step `PlanStep.model_validate`. Same enrichment rules at the end
(empty-plan synthesis, appending an answering `llm` step when the model
ends on a data step).

Both paths feed the same `_enforce_plan_shape` post-parse enrichment, so
the on-wire `plan` frame is identical regardless of which path produced it.
"""

from __future__ import annotations

import json
import logging

from langchain_core.callbacks import UsageMetadataCallbackHandler
from langchain_core.exceptions import OutputParserException
from langchain_core.messages import HumanMessage, SystemMessage
from langchain_core.output_parsers import JsonOutputParser
from langchain_core.runnables import RunnableLambda
from langchain_core.runnables.config import ensure_config, merge_configs
from langchain_openai import ChatOpenAI
from opentelemetry import trace
from pydantic import SecretStr, ValidationError

from .config import Settings
from .observability import (
    agent_planner_parse_failures_total,
    agent_tokens_total,
    jaeger_span,
    trace_observation,
    update_observation,
)
from .state import AgentState, accumulate_usage, accumulate_usage_metadata
from .tool_dispatch import ToolDispatcher, registry_planner_block
from .types import (
    PLAN_LEVEL_ERROR_CODES,
    DoneEvent,
    DoneUsage,
    ErrorEvent,
    Event,
    NoteEvent,
    Plan,
    PlanEvent,
    PlanStep,
    StepKind,
    TerminatorEvent,
)
from .web_tools import planner_block

log = logging.getLogger("nyx.agent.planner")

PLANNER_SYSTEM_PROMPT = """You are a planner that decomposes a user's chat request into an ordered sequence of agent steps. The steps are executed sequentially by an orchestrator that supports four kinds:

- rag:    vector retrieval over the user's feed corpus; { query: <str> }
- tool:   call a registered tool by name; { tool: <name>, args: <object> }
- llm:    a single LLM completion; { prompt: <str> }
- react:  a nested model-tool loop that ends when the model writes its result text, capped by max_iterations; { prompt: <objective>, max_iterations: <int> }

Return ONLY a JSON object matching this schema:
{{"steps": [{{"id": "<step_id>", "kind": "rag|tool|llm|react", "tool": "<name>", "args": {{...}}, "prompt": "<text>", "query": "<text>", "max_iterations": <int>}}, ...]}}

Rules:
- step `id` MUST be unique within the plan and stable across the run
- plan for only the MOST RECENT user message; earlier messages are context, never re-answer them
- `rag` and `tool` steps gather data and emit NO user-visible text; the plan MUST end with an `llm` step that writes the final answer using the data gathered by earlier steps
- a `react` step MUST carry a `prompt` stating its objective — it is the loop's system instruction, and the text the loop writes is user-visible
- the plan MUST contain at least one step; for greetings, small talk, or anything needing no retrieval or tools, emit exactly one `llm` step that answers the user
- prefer the minimum number of steps; don't insert filler
- use `rag` only when the request is clearly about the user's own data
- when unsure, prefer a single `llm` step
"""

# Sentinel marker for the appended validator-error block. The model keys off
# "your previous attempt failed" rather than "please fix", which is how the
# underlying providers bias prompt-following on self-correction.
PREVIOUS_ATTEMPT_FAILED_HEADER = "PREVIOUS_ATTEMPT_FAILED"

# Legacy fallback: sentinel markers we accept when the upstream wraps JSON in fences.
_FENCE_PREFIX = "```"


class _PlanError:
    def __init__(self, code: str, detail: str, reason: str = "") -> None:
        self.code = code
        self.detail = detail
        self.reason = reason


async def plan_node(
    state: AgentState,
    settings: Settings,
    tracer: trace.Tracer,
    prefs_text: str = "",
    tools: ToolDispatcher | None = None,
) -> AgentState:
    """Invoke the planner LLM; mutate state.plan and state.events_so_far.

    Whether structured-output binding is the primary path is governed by
    `settings.planner_structured_enabled`. The wire-format output is the
    same either way — the client observes a single `plan` SSE frame.

    `prefs_text` is the rendered per-user preferences block
    (openspec/changes/agent-user-preferences D5) appended to the system
    prompt. It is `""` when the user has no prefs, the kill switch is
    off, or the load failed — the composed prompt is then byte-identical
    to its pre-injection form.

    `tools` is the run's dispatcher; when present, the live registry
    catalog block (openspec/changes/agent-feed-tools D4) is appended after
    the prefs block, best-effort — a failed or empty registry call leaves
    the prompt byte-identical to its pre-change form (spec: "Registry
    outage leaves planning unchanged").
    """
    # Registry catalog (agent-feed-tools D4), fetched once per plan; the
    # dispatcher memoizes the underlying tools/list for the whole run.
    registry_block = ""
    if tools is not None:
        try:
            registry_block = registry_planner_block(await tools.registry_tools())
        except Exception as exc:  # noqa: BLE001 — catalog is best-effort
            log.warning("planner: registry catalog fetch failed: %r", exc)
    with (
        # Langfuse twin of the plan span (agent-langfuse-tracing 3.4).
        # Entered BEFORE the Jaeger span (parent-before-child); the Jaeger
        # span parents explicitly via jaeger_span so ambient-context noise
        # can't re-root the Jaeger tree.
        trace_observation(
            "agent.step.plan",
            # Same dual-shape accessor as `_messages_to_prompt` below:
            # production state carries ChatMessage models, unit tests
            # inject plain dicts.
            input={
                "messages": [
                    (
                        {"role": m.role, "content": m.content}
                        if hasattr(m, "role")
                        else {"role": m.get("role"), "content": m.get("content")}
                    )
                    for m in state.get("messages", [])
                ]
            },
            metadata={
                "kind": "plan",
                "structured": settings.planner_structured_enabled,
            },
        ) as lf_plan,
        jaeger_span(tracer, "agent.step.plan") as span,
    ):
        span.set_attribute("step_id", "plan")
        span.set_attribute("kind", "plan")
        span.set_attribute("iteration", 0)
        span.set_attribute("structured", settings.planner_structured_enabled)

        if settings.planner_structured_enabled:
            plan_or_error = await _emit_structured_plan(
                state, settings, tracer, prefs_text, registry_block=registry_block
            )
        else:
            plan_or_error = await _emit_legacy_plan(
                state, settings, tracer, prefs_text, registry_block=registry_block
            )

        if isinstance(plan_or_error, _PlanError):
            update_observation(
                lf_plan,
                output={"status": "failed", "code": plan_or_error.code},
                level="ERROR",
            )
            return _emit_error(
                state,
                code=plan_or_error.code,
                detail=plan_or_error.detail,
                reason=plan_or_error.reason,
            )

        state["plan"] = plan_or_error
        state["events_so_far"].append(
            Event(
                kind="plan", payload=PlanEvent(steps=plan_or_error.steps).model_dump()
            )
        )
        update_observation(
            lf_plan,
            output={
                "status": "ok",
                "steps": [s.model_dump() for s in plan_or_error.steps],
            },
        )
        return state


# --- structured-output path --------------------------------------------


def _build_structured_client(settings: Settings):
    """Bind a `Plan` response schema onto a fresh ChatOpenAI client.

    Deliberately NOT `ChatOpenAI.with_structured_output(Plan,
    method="json_schema", strict=False)`: verified against
    langchain-openai 1.7.0, that path silently forces `strict: True` onto
    the wire for Pydantic-class schemas — `_convert_to_openai_response_format`
    returns the class unchanged at bind time (dropping the caller's
    `strict=False`), and `bind()` then defaults `strict = True` whenever a
    `response_format` is present. Strict mode forbids
    `additionalProperties: true` on objects, which makes the free-form
    `PlanStep.args` object unrepresentable: the API either rejects the
    schema or the decoder resolves `args` to `{}` (tool-argument loss).

    So we bind the `response_format` dict ourselves — the dict-unwrap path
    in `bind()` honours the embedded `strict: False` — and validate the
    returned content JSON with Pydantic. `JsonOutputParser` raises
    `OutputParserException` on non-JSON content (e.g. a provider that
    answers with `tool_calls` instead of content), which the retry loop
    treats as a retryable conformance failure. vLLM ignores `strict` and
    honours the schema via guided decoding; when its parser cannot, set
    `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false` to drop back to the
    legacy free-text path.
    """
    base = ChatOpenAI(
        base_url=settings.llm_base_url,
        api_key=SecretStr(settings.llm_api_key or "no-key"),
        model=settings.planner_model,
        temperature=0.0,
        # Transport field stays `max_tokens` (vLLM-compatible wire);
        # langchain-openai 1.7 types only the renamed field, so mypy
        # sees a mismatch that does not exist at runtime.
        max_tokens=512,  # type: ignore[call-arg]
    )
    response_format = {
        "type": "json_schema",
        "json_schema": {
            "name": "Plan",
            "schema": Plan.model_json_schema(),
            "strict": False,
        },
    }
    return (
        base.bind(response_format=response_format)
        | JsonOutputParser()
        | RunnableLambda(lambda d: Plan.model_validate(d))
    )


def _initial_messages(
    state: AgentState,
    settings: Settings,
    prefs_text: str = "",
    registry_block: str = "",
) -> list:
    """The planner's opening messages.

    The system prompt is the static base plus the conditional web-tools
    block (openspec/changes/agent-web-tools D9) — the block is "" when
    both web-tool flags are off, keeping the prompt byte-identical to its
    pre-web-tools form.

    The per-user preferences block (openspec/changes/agent-user-preferences
    D5) appends after `planner_block` — the static base stays first
    (providers cache the static prefix better), and `prefs_text` is `""`
    whenever the user has no prefs, so the composition is then
    byte-identical to its pre-injection form (regression-pinned in
    `tests/test_prefs_injection.py`).

    The registry catalog block (openspec/changes/agent-feed-tools D4)
    appends last; it is `""` when the registry list is empty or its fetch
    failed, so the prompt is then byte-identical to its pre-change form
    (pinned in `tests/test_planner_catalog_wiring.py`).
    """
    return [
        SystemMessage(
            content=(
                PLANNER_SYSTEM_PROMPT
                + planner_block(settings)
                + (prefs_text or "")
                + (registry_block or "")
            )
        ),
        HumanMessage(content=_messages_to_prompt(state.get("messages", []))),
    ]


def _append_error_to_messages(messages: list, error: str) -> list:
    """Re-prompt with the validator's last error appended to the user message.

    The error is appended as a user-side correction (not a new system
    instruction) so the schema instructions stay stable across retries —
    the model sees "your previous attempt failed" feedback alongside its
    original request rather than a moving schema target.

    Only the LAST message is mutated, so the system message — including
    the web-tools and preferences blocks — rides along unchanged on every
    retry.
    """
    err_block = f"\n\n{PREVIOUS_ATTEMPT_FAILED_HEADER}\n{error}"
    msgs = list(messages)
    last = msgs[-1]
    last.content = (last.content or "") + err_block
    return msgs


def _extract_structured_error(exc: Exception) -> str:
    """Normalize parser/validation exceptions to a short string we can inject."""
    if isinstance(exc, ValidationError):
        return _format_validation_error(exc)
    if isinstance(exc, OutputParserException):
        # langchain-core stores the parser message in `args[0]`, not an
        # `error` attribute (verified against langchain-core 1.6.9 —
        # `hasattr(exc, "error")` is False). `str(exc)` appends a
        # "For troubleshooting, visit …" doc link; strip it so the
        # re-prompt carries just the parser message.
        return str(exc).split("\nFor troubleshooting")[0][:500]
    return repr(exc)[:500]


def _format_validation_error(exc: ValidationError) -> str:
    parts: list[str] = []
    for err in exc.errors():
        loc = ".".join(str(x) for x in err.get("loc", ()))
        msg = err.get("msg", "")
        parts.append(f"{loc}: {msg}")
    return "; ".join(parts)[:500]


async def _emit_structured_plan(
    state: AgentState,
    settings: Settings,
    tracer: trace.Tracer,
    prefs_text: str = "",
    registry_block: str = "",
) -> Plan | _PlanError:
    """Bound the structured-output retry loop. Returns `Plan` or `_PlanError`.

    The outer loop runs `planner_retry_max + 1` attempts total: the first
    attempt plus up to `planner_retry_max` retries. Each attempt logs an
    OTel span (`agent.planner.structured` for the first, `agent.planner.retry`
    thereafter).

    Counter semantics (per the spec scenarios): `retry="exhausted"` ticks only
    when the run terminates with `plan_invalid` (schema/validation failures);
    `retry="ok"` ticks when a validation failure was recovered by a later
    attempt. A terminal `plan_too_large` preserves its typed code and does
    not tick the parse-failure counter.
    """
    try:
        client = _build_structured_client(settings)
    except Exception as exc:  # noqa: BLE001 — construction fault is startup-class
        # e.g. an old `langchain-openai` without `response_format`
        # json_schema support, or an invalid base_url. Historical
        # behaviour contract: typed `planner_failed` error, never an
        # exception escaping the node.
        # The raw repr stays in the LOG (and OTel), never on the wire:
        # provider errors can echo credential fragments (e.g. "Incorrect
        # API key provided: sk-…"), and `_emit_error` forwards `reason`
        # into the terminator frame that old clients render.
        log.warning("planner client construction failed: %r", exc)
        return _PlanError("planner_failed", "Plan generation failed", "provider call failed")

    messages = _initial_messages(state, settings, prefs_text, registry_block)
    had_validation_failure = False

    for attempt in range(settings.planner_retry_max + 1):
        span_name = (
            "agent.planner.structured" if attempt == 0 else "agent.planner.retry"
        )
        with jaeger_span(tracer, span_name) as span:
            span.set_attribute("attempt", attempt)
            usage_cb = UsageMetadataCallbackHandler()
            try:
                cfg = merge_configs(ensure_config(), {"callbacks": [usage_cb]})
                plan_obj = await client.ainvoke(messages, config=cfg)
            except (OutputParserException, ValidationError, ValueError) as exc:
                _accrue_callback_usage(state, usage_cb, settings)
                # Schema-conformance / parse failure — retryable.
                # (OutputParserException and ValidationError both subclass
                # ValueError; the explicit names document intent. A bare
                # ValueError from the client stack is retried too — the
                # one-shot retry bounds the cost of the occasional
                # misclassification. Non-ValueError provider faults —
                # auth, network, API-status errors — do NOT match here
                # and fall through to the `planner_failed` branch.)
                last_error = _extract_structured_error(exc)
                had_validation_failure = True
                span.set_attribute("ok", False)
                span.set_attribute("error", last_error[:200])
                if attempt >= settings.planner_retry_max:
                    agent_planner_parse_failures_total.labels(retry="exhausted").inc()
                    return _PlanError(
                        "plan_invalid",
                        "Plan is invalid",
                        last_error or "validation failed",
                    )
                messages = _append_error_to_messages(messages, last_error)
                continue
            except Exception as exc:  # noqa: BLE001 — non-retryable provider fault
                # Parity with the legacy path: upstream faults (auth,
                # network, a provider that rejects `response_format`)
                # surface as the typed `planner_failed` error instead of
                # escaping the graph node. Retrying these is pointless.
                _accrue_callback_usage(state, usage_cb, settings)
                span.set_attribute("ok", False)
                span.set_attribute("error", repr(exc)[:200])
                log.warning("planner provider call failed: %r", exc)
                return _PlanError(
                    "planner_failed", "Plan generation failed", "provider call failed"
                )

            _accrue_callback_usage(state, usage_cb, settings)
            if not isinstance(plan_obj, Plan):
                # Defensive: the bound chain (JsonOutputParser →
                # Plan.model_validate) raises rather than returning a
                # non-Plan, but a swapped chain — or a future regression
                # to `with_structured_output`, which CAN return None for
                # tool_calls-bearing responses — must not let an
                # AttributeError escape the graph node and abort the SSE
                # stream mid-flight.
                last_error = (
                    f"structured output returned {type(plan_obj).__name__}; "
                    "expected a Plan object"
                )
                had_validation_failure = True
                span.set_attribute("ok", False)
                span.set_attribute("error", last_error[:200])
                if attempt >= settings.planner_retry_max:
                    agent_planner_parse_failures_total.labels(retry="exhausted").inc()
                    return _PlanError("plan_invalid", "Plan is invalid", last_error)
                messages = _append_error_to_messages(messages, last_error)
                continue

            enriched = _enforce_plan_shape(plan_obj, settings.plan_max_steps)
            if isinstance(enriched, _PlanError):
                reason = enriched.reason or enriched.code
                span.set_attribute("ok", False)
                span.set_attribute("error", reason[:200])
                if attempt >= settings.planner_retry_max:
                    # Preserve the typed code (`plan_too_large` vs
                    # `plan_invalid`); `exhausted` ticks only for
                    # `plan_invalid` terminations.
                    if enriched.code == "plan_invalid":
                        agent_planner_parse_failures_total.labels(
                            retry="exhausted"
                        ).inc()
                    return enriched
                if enriched.code == "plan_invalid":
                    had_validation_failure = True
                messages = _append_error_to_messages(messages, reason)
                continue

            if had_validation_failure:
                agent_planner_parse_failures_total.labels(retry="ok").inc()
            return enriched

    # Unreachable: every loop iteration returns. Defensive for totality.
    return _PlanError("planner_failed", "Plan generation failed", "retry loop exited")


# --- legacy free-text fallback ------------------------------------------


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


def _accrue_callback_usage(
    state: AgentState, cb: UsageMetadataCallbackHandler, settings: Settings
) -> None:
    """Fold the handler's per-model usage totals into the run's state counters.

    The handler only records when the response carries `model_name` (real
    providers do; stubs may not), so absent telemetry is a no-op. Failed
    attempts accrue too — the provider billed them.
    """
    total = 0
    for meta in cb.usage_metadata.values():
        _prompt, _completion, _total = accumulate_usage_metadata(state, meta)
        total += _total
    if total:
        agent_tokens_total.labels("planner", settings.planner_model).inc(total)


async def _emit_legacy_plan(
    state: AgentState,
    settings: Settings,
    tracer: trace.Tracer,
    prefs_text: str = "",
    registry_block: str = "",
) -> Plan | _PlanError:
    """Legacy free-text path: fence-strip + `json.loads` + post-parse enrichment.

    Reachable only when `PYTHON_AGENT_PLANNER_STRUCTURED_ENABLED=false`. Kept
    symmetric with the structured path on the post-parse shape — both paths
    call `_enforce_plan_shape` so the wire-format output is identical.
    """
    client = ChatOpenAI(
        base_url=settings.llm_base_url,
        api_key=SecretStr(settings.llm_api_key or "no-key"),
        model=settings.planner_model,
        temperature=0.0,
        # Transport field stays `max_tokens` (vLLM-compatible wire);
        # langchain-openai 1.7 types only the renamed field, so mypy
        # sees a mismatch that does not exist at runtime.
        max_tokens=512,  # type: ignore[call-arg]
    )
    with jaeger_span(tracer, "agent.planner.legacy") as span:
        span.set_attribute("structured", False)
        try:
            resp = await client.ainvoke(
                _initial_messages(state, settings, prefs_text, registry_block)
            )
        except Exception as exc:  # noqa: BLE001 — surface as a typed error event
            log.warning("planner provider call failed (legacy path): %r", exc)
            return _PlanError(
                "planner_failed", "Plan generation failed", "provider call failed"
            )

        _prompt, _completion, _total = accumulate_usage(state, resp)
        if _total:
            agent_tokens_total.labels("planner", settings.planner_model).inc(_total)
        raw = _extract_text(resp)
        plan_or_error = _parse_and_validate(raw, max_steps=settings.plan_max_steps)
        if (
            isinstance(plan_or_error, _PlanError)
            and plan_or_error.code == "plan_invalid"
        ):
            # Spec: `exhausted` counts runs that terminate with `plan_invalid`.
            # The legacy path has no retry, so a parse failure is terminal
            # immediately.
            agent_planner_parse_failures_total.labels(retry="exhausted").inc()
        return plan_or_error


def _parse_and_validate(raw: str, max_steps: int) -> Plan | _PlanError:
    """Legacy free-text parser. See `tests/test_planner.py` for behaviour."""
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
        # user-visible answer — synthesize the canonical direct-answer step.
        steps = [
            {
                "id": "step_1",
                "kind": "llm",
                "prompt": "Answer the user's message directly.",
            }
        ]
    if len(steps) > max_steps:
        return _PlanError(
            "plan_too_large", "Plan exceeds maximum size", f"{len(steps)} > {max_steps}"
        )

    seen_ids: set[str] = set()
    parsed_steps: list[PlanStep] = []
    try:
        for s in steps:
            parsed = PlanStep.model_validate(s)
            if parsed.id in seen_ids:
                return _PlanError(
                    "plan_invalid", "Plan is invalid", f"duplicate step id {parsed.id!r}"
                )
            seen_ids.add(parsed.id)
            parsed_steps.append(parsed)

        if parsed_steps[-1].kind.value not in ("llm", "react"):
            answer_id = "final_answer"
            n = 2
            while answer_id in seen_ids:
                answer_id = f"final_answer_{n}"
                n += 1
            log.info(
                "planner: model plan ended with a %s step; appended answering step %r",
                parsed_steps[-1].kind.value,
                answer_id,
            )
            parsed_steps.append(
                PlanStep(
                    id=answer_id,
                    kind=StepKind.llm,
                    prompt=(
                        "Write the final answer to the user's most recent "
                        "request, using the data gathered by earlier steps."
                    ),
                )
            )
        return Plan(steps=parsed_steps)
    except Exception as exc:  # noqa: BLE001
        return _PlanError("plan_invalid", "Plan is invalid", repr(exc))


# --- post-parse enrichment (shared) -------------------------------------


def _enforce_plan_shape(plan: Plan, max_steps: int) -> Plan | _PlanError:
    """Same enrichment as the legacy parser, on a typed `Plan` instance.

    - Empty plans become a single direct-answer `llm` step (small models
      deterministically return `{"steps": []}` for greetings).
    - Plans exceeding `max_steps` are rejected with `plan_too_large`.
    - Duplicate step ids are rejected with `plan_invalid`.
    - Plans whose last step is a data step (`rag`, `tool`) get a final
      answering `llm` step appended so the run produces a user-visible
      answer (the 2026-10 "summarize my feeds" bug).
    """
    steps = list(plan.steps)
    if not steps:
        # Same fallback as `_parse_and_validate` empty-plan branch — keep
        # the wire-format identical between the two paths.
        steps = [
            PlanStep(
                id="step_1",
                kind=StepKind.llm,
                prompt="Answer the user's message directly.",
            )
        ]

    if len(steps) > max_steps:
        return _PlanError(
            "plan_too_large",
            "Plan exceeds maximum size",
            f"{len(steps)} > {max_steps}",
        )

    seen_ids: set[str] = set()
    for s in steps:
        if s.id in seen_ids:
            return _PlanError(
                "plan_invalid", "Plan is invalid", f"duplicate step id {s.id!r}"
            )
        seen_ids.add(s.id)

    if steps[-1].kind.value not in ("llm", "react"):
        answer_id = "final_answer"
        n = 2
        while answer_id in seen_ids:
            answer_id = f"final_answer_{n}"
            n += 1
        log.info(
            "planner: model plan ended with a %s step; appended answering step %r",
            steps[-1].kind.value,
            answer_id,
        )
        steps.append(
            PlanStep(
                id=answer_id,
                kind=StepKind.llm,
                prompt=(
                    "Write the final answer to the user's most recent "
                    "request, using the data gathered by earlier steps."
                ),
            )
        )
    return Plan(steps=steps)


def _emit_error(
    state: AgentState, code: str, detail: str, reason: str = ""
) -> AgentState:
    """Emit a typed error event then a done event then a terminator event.

    Wire shapes per `frontend/src/features/chat/services/chatService.ts:232-258, 185-215, 169-171`:
      - error:        `{"error": "<detail>", "code": "<code>", "request_id": "<rid>"}`
      - done:         `{"usage": {"prompt_tokens", "completion_tokens", "total_tokens"}}`
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
    # agent-tool-dispatch-retry (spec: agent-partial-failure): a plan-level
    # failure is a plan divergence. The note is emitted HERE, in the same
    # planner chunk as error/done — `run_agent_stream` slices
    # `events_so_far` positionally per node chunk, so a note inserted into
    # this already-yielded range later (e.g. from the executor node) would
    # never reach the wire. No step ran, so `failed_steps` is empty.
    # Gated on the spec's CLOSED trigger set: `planner_failed` is a
    # provider/construction fault, not a plan divergence (round 2, NEW-2).
    if code in PLAN_LEVEL_ERROR_CODES:
        state["events_so_far"].append(
            Event(
                kind="note",
                payload=NoteEvent(
                    text="Run diverged from the plan",
                    kind="partial_failure",
                    failed_steps=[],
                ).model_dump(),
            )
        )
    state["events_so_far"].append(
        Event(
            kind="done",
            payload=DoneEvent(
                usage=DoneUsage(
                    prompt_tokens=state.get("cumulative_prompt_tokens", 0),
                    completion_tokens=state.get("cumulative_completion_tokens", 0),
                    total_tokens=state.get("cumulative_tokens", 0),
                )
            ).model_dump(),
        )
    )
    state["events_so_far"].append(
        Event(kind="terminator", payload=TerminatorEvent(reason=reason).model_dump())
    )
    return state
