"""Per-run effective LLM/Tavily configuration (openspec/changes/add-user-llm-byok).

The Go chat handler forwards a verified user's BYOK config in the
`/agent/run` payload (`ChatRequest.llm_config` / `tavily_config`). This
module is the seam between that payload and the `ChatOpenAI`/web-tool
construction sites:

- `run_overrides_from_request` splits a request into the checkpoint-safe
  state views (`LLMOverrideView` / `TavilyOverrideView`) and the
  request-scoped key holder (`OverrideSecrets`).
- `resolve_llm` collapses state + settings + holder into the
  (base_url, api_key, model) triple a client construction site needs.
- `structured_supported` is the per-run planner emission-path heuristic.

D11 (binding): the production checkpointer (`AsyncPostgresSaver`) writes
every `AgentState` channel into the `langgraph` schema verbatim on each
node transition, so the plaintext keys NEVER ride the state — they live
only in `OverrideSecrets`, threaded to the nodes via the per-run graph
closure (and to the detached title/prefs tasks as plain parameters). The
Langfuse/OTel scrubber only matches Bearer/JWT/`sk-`/`*-lf-` shapes, so
the never-log guarantee is also by construction: the key is never passed
to any logging or tracing call, only to client constructors.

Precedence (per the change spec):

- override present: its base_url and api_key win at every site; the
  planner model is the override's model (was `settings.planner_model`);
  executor steps keep `state.model > override.model > settings.llm_model`.
- no override: byte-identical legacy behavior (Settings fallback).

A state that carries an override view but no holder key (e.g. a run
resumed from a checkpoint recorded by a request that is long gone)
resolves the override endpoint with the `"no-key"` placeholder — the
provider's auth failure is the visible outcome, never a silent redirect
of the user's traffic to the deployment's own endpoint.
"""

from __future__ import annotations

import uuid
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlparse

from .config import Settings
from .types import ChatRequest, LLMOverrideView, TavilyOverrideView

NO_KEY_PLACEHOLDER = "no-key"

LLM_OVERRIDE_STATE_KEY = "llm_override"


def override_only_state(view: LLMOverrideView | None) -> dict[str, Any]:
    """A minimal resolve_llm-compatible state for call sites outside the graph.

    The title-refinement and prefs-extraction tasks run detached from the
    run handler and hold no `AgentState`; they carry the override view
    (plus the holder) instead, and resolution sees exactly the state shape
    `new_state` produces for the same request: the view present, no
    per-request `model` override.
    """
    return {LLM_OVERRIDE_STATE_KEY: view, "model": None}


@dataclass(frozen=True)
class OverrideSecrets:
    """Request-scoped holder for override API key material (D11).

    One instance per run, minted by `run_overrides_from_request` and
    threaded via `build_graph`'s per-run closure into the planner/executor
    nodes, and as a plain parameter into the detached title/prefs tasks.
    Deliberately a plain object, not a `contextvars.ContextVar`: langGraph
    may execute nodes in task contexts a request-scoped contextvar cannot
    be verified against, while the closure is deterministic. Never logged,
    never traced, never placed on `AgentState` — the handle is the only
    trace of it in the checkpoint.
    """

    handle: str
    llm_api_key: str | None = None
    tavily_api_key: str | None = None


@dataclass(frozen=True)
class EffectiveLLM:
    """The (base_url, api_key, model) triple for one client construction.

    `api_key` is only ever the deployment key (no override) or the value
    handed in as `api_key_from_holder` — this module has no other source.
    """

    base_url: str
    api_key: str
    model: str


def run_overrides_from_request(
    chat_req: ChatRequest,
) -> tuple[OverrideSecrets, LLMOverrideView | None, TavilyOverrideView | None]:
    """Split a request's BYOK config into holder + checkpoint-safe views.

    The handle is a random uuid4 hex — opaque, unrelated to the key
    material, stable across both views so a run's checkpoint names its
    holder without describing it.
    """
    handle = uuid.uuid4().hex
    llm_view: LLMOverrideView | None = None
    tavily_view: TavilyOverrideView | None = None
    llm_key: str | None = None
    tavily_key: str | None = None
    if chat_req.llm_config is not None:
        llm_view = LLMOverrideView(
            handle=handle,
            base_url=chat_req.llm_config.base_url,
            model=chat_req.llm_config.model,
        )
        llm_key = chat_req.llm_config.api_key
    if chat_req.tavily_config is not None:
        tavily_view = TavilyOverrideView(
            handle=handle,
            base_url=chat_req.tavily_config.base_url,
        )
        tavily_key = chat_req.tavily_config.api_key
    secrets = OverrideSecrets(
        handle=handle, llm_api_key=llm_key, tavily_api_key=tavily_key
    )
    return secrets, llm_view, tavily_view


def llm_holder_handle(secrets: OverrideSecrets | None) -> str | None:
    return secrets.handle if secrets is not None else None


def llm_holder_key(secrets: OverrideSecrets | None) -> str | None:
    """The override LLM key from the holder, or None without a holder.

    The single sanctioned key source for `resolve_llm` call sites: a
    caller holding only the state cannot reach key material.
    """
    return secrets.llm_api_key if secrets is not None else None


def resolve_llm_endpoint(
    state: Mapping[str, Any], settings: Settings, *, planner: bool
) -> tuple[str, str]:
    """Resolve (base_url, model) from the state alone — never a key.

    Planner sites use the override's model when present (the user's model
    governs plan emission too, add-user-llm-byok D9); executor sites keep
    `state.model > override.model > settings.llm_model`.
    """
    view = state.get("llm_override")
    if view is None:
        model = (
            settings.planner_model
            if planner
            else (state.get("model") or settings.llm_model)
        )
        return settings.llm_base_url, model
    if planner:
        model = view.model or settings.planner_model
    else:
        model = state.get("model") or view.model or settings.llm_model
    return view.base_url, model


def resolve_llm(
    state: Mapping[str, Any],
    settings: Settings,
    *,
    planner: bool,
    api_key_from_holder: str | None,
    holder_handle: str | None = None,
) -> EffectiveLLM:
    """Resolve the full client triple; the key comes only from the holder.

    Without an override the deployment Settings apply unchanged (legacy
    byte-identical behavior, including the `or "no-key"` empty-key
    guard). With an override, the holder's key applies against the
    override's endpoint; a missing holder key resolves to
    `NO_KEY_PLACEHOLDER` so the provider — not a settings fallback —
    owns the visible failure.
    """
    base_url, model = resolve_llm_endpoint(state, settings, planner=planner)
    view = state.get("llm_override")
    if view is None:
        return EffectiveLLM(
            base_url=base_url,
            api_key=settings.llm_api_key or NO_KEY_PLACEHOLDER,
            model=model,
        )
    api_key = api_key_from_holder or NO_KEY_PLACEHOLDER
    if holder_handle is not None and getattr(view, "handle", None) != holder_handle:
        api_key = NO_KEY_PLACEHOLDER
    return EffectiveLLM(base_url=base_url, api_key=api_key, model=model)


def structured_supported(base_url: str | None, settings: Settings) -> bool:
    """Whether this run's planner may use the structured-output binding.

    Per-run heuristic (add-user-llm-byok D9): the deployment-wide flag AND
    — only when the run carries an override — a host check against the
    override's base URL (URL heuristics are imperfect; the planner's
    bounded self-check retry remains the safety net). No override keeps
    the legacy global-flag-only selection, byte-identical.
    """
    if not settings.planner_structured_enabled:
        return False
    if not base_url:
        return True
    host = urlparse(base_url).hostname or ""
    return "deepseek" not in host.lower()
