"""Local tool registry + the executor's composite tool dispatcher
(openspec/changes/agent-web-tools, design.md D1–D3).

`ToolDispatcher` presents the exact surface the executor already consumes
(`list_tools()` / `call(name, args)`) over two tool sources:

  - **agent-native tools** (`LocalToolRegistry`, built from settings at
    startup — web tools today). Dispatched first; never exposed via MCP.
  - **the MCP registry** (the per-request `ToolsClient`, bearer-scoped).

Merge semantics (spec: agent-web-tools » funnel requirement):

  - `list_tools()` returns local schemas first, then MCP schemas;
  - a name collision keeps the LOCAL tool and drops the MCP entry (warned
    once per run) — the MCP tool stays reachable only under a
    non-colliding name;
  - when MCP `tools/list` fails, the dispatcher degrades to local-only
    (logged) instead of the pre-web-tools empty list — a registry outage
    no longer blinds a react loop that could still use local tools.

Registry-sourced schemas are normalized at this boundary
(openspec/changes/agent-feed-tools D1): MCP-native
`{name, description, inputSchema, annotations}` entries become bindable
function tools (`inputSchema` → `parameters`), with annotations kept on a
side map — one shape for every consumer (react binding, retry schema
text, planner catalog, confirmation gate). The registry fetch is
memoized per dispatcher instance (≈ per run), so all of those consumers
share one `tools/list` call (task 2.4).

Local tools raise the same exception types the MCP client raises
(`ToolError` / `ToolUnavailableError` / `ToolRejectedError`), so
`executor._classify_tool_failure` works unchanged (design.md D2).
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any

from .tools_client import ToolsClient, normalize_registry_tools

log = logging.getLogger("nyx.agent.tool_dispatch")


@dataclass(frozen=True)
class LocalTool:
    """An agent-native tool: OpenAI function schema + async dispatcher."""

    name: str
    schema: dict[str, Any]
    dispatch: Callable[[dict], Awaitable[dict]]


class LocalToolRegistry:
    """Name-keyed registry of agent-native tools (immutable during a run)."""

    def __init__(self, tools: list[LocalTool] | None = None) -> None:
        self._tools: dict[str, LocalTool] = {}
        for tool in tools or []:
            self.register(tool)

    def register(self, tool: LocalTool) -> None:
        if tool.name in self._tools:
            # Duplicate registration is a programming error, not a runtime
            # condition — refuse loudly at startup.
            raise ValueError(f"duplicate local tool registration: {tool.name!r}")
        self._tools[tool.name] = tool

    def __contains__(self, name: str) -> bool:
        return name in self._tools

    def __len__(self) -> int:
        return len(self._tools)

    def get(self, name: str) -> LocalTool:
        return self._tools[name]

    def schemas(self) -> list[dict[str, Any]]:
        return [tool.schema for tool in self._tools.values()]


def _schema_name(schema: Any) -> str:
    """Tool name from an OpenAI-format schema (`{type, function:{name}}`),
    tolerating MCP-native shapes that carry a top-level `name`."""
    if isinstance(schema, dict):
        function = schema.get("function")
        if isinstance(function, dict) and isinstance(function.get("name"), str):
            return function["name"]
        if isinstance(schema.get("name"), str):
            return schema["name"]
    return ""


class ToolDispatcher:
    """Composite tool source: local (agent-native) first, MCP fallback.

    Instantiated per request (one instance ≈ one run), which is what makes
    the collision warning "once per run" and the registry memo (below)
    "once per run".
    """

    def __init__(self, mcp: ToolsClient, local: LocalToolRegistry | None = None) -> None:
        self._mcp = mcp
        self._local = local if local is not None else LocalToolRegistry()
        self._collisions_warned: set[str] = set()
        # Per-run registry memo (openspec/changes/agent-feed-tools D4/task
        # 2.4): one `tools/list` fetch feeds the planner catalog, react
        # binding, retry schema text, and the confirmation gate. A FAILED
        # fetch is deliberately NOT memoized — a transient outage must not
        # poison the rest of the run; the next consumer retries it.
        self._registry_schemas: list[dict] | None = None
        self._registry_annotations: dict[str, dict] | None = None

    async def _load_registry(self) -> tuple[list[dict], dict[str, dict]] | None:
        """Fetch + normalize the MCP registry once per run (memoized).

        Returns None when the fetch fails — callers degrade: `list_tools`
        falls back to local-only, the planner block is omitted, and the
        confirmation gate treats the run as having no known registry
        tools. A successful fetch with an empty list is memoized as such
        (legitimately no tools — not a failure).
        """
        if self._registry_schemas is None or self._registry_annotations is None:
            try:
                raw = await self._mcp.list_tools()
            except Exception as exc:  # noqa: BLE001 — degrade, never fail the list
                log.warning("MCP tools/list failed: %r", exc)
                return None
            self._registry_schemas, self._registry_annotations = (
                normalize_registry_tools(raw)
            )
        return self._registry_schemas, self._registry_annotations

    async def list_tools(self) -> list[dict]:
        local_schemas = self._local.schemas()
        local_names = {_schema_name(s) for s in local_schemas}

        loaded = await self._load_registry()
        if loaded is None:
            if local_schemas:
                log.warning("MCP tools/list failed; serving agent-native tools only")
            return local_schemas
        mcp_schemas, _ = loaded

        merged = list(local_schemas)
        for schema in mcp_schemas:
            name = _schema_name(schema)
            if name and name in local_names:
                if name not in self._collisions_warned:
                    self._collisions_warned.add(name)
                    log.warning(
                        "tool name %r exists in both the agent-native registry "
                        "and the MCP registry; the agent-native tool wins and the "
                        "MCP entry is hidden",
                        name,
                    )
                continue
            merged.append(schema)
        return merged

    async def registry_tools(self) -> list[dict]:
        """Registry-only view (normalized, local-shadowed names excluded).

        The planner catalog source (design D4): the merged list minus
        agent-native tools. Returns [] when the registry call fails, so
        the planner block is omitted and the prompt stays byte-identical
        to its pre-change form.
        """
        loaded = await self._load_registry()
        if loaded is None:
            return []
        schemas, _ = loaded
        local_names = {_schema_name(s) for s in self._local.schemas()}
        return [s for s in schemas if _schema_name(s) not in local_names]

    async def confirmation_required(self, name: str) -> bool:
        """Whether a call to `name` must be confirmed before dispatch.

        Fail-safe policy (design D3): a tool in the registry view whose
        annotations do not assert `readOnlyHint=true` — including absent
        or empty annotations — requires confirmation. Agent-native tools
        never gate (the gate is a registry-tool policy). A name absent
        from the registry view is not a registry tool (dispatch rejects
        it naturally), so it does not gate; when the registry fetch
        itself failed, no name is known — the model was never bound
        registry tools in that state either.
        """
        if name in self._local:
            return False
        loaded = await self._load_registry()
        if loaded is None:
            return False
        _, annotations = loaded
        entry = annotations.get(name)
        if entry is None:
            return False
        return entry.get("readOnlyHint") is not True

    async def call(self, name: str, arguments: dict) -> dict:
        if name in self._local:
            return await self._local.get(name).dispatch(arguments)
        return await self._mcp.call(name, arguments)


# --- planner awareness (openspec/changes/agent-feed-tools D4) ---------------

_REGISTRY_PLANNER_HEADER = "Registry tools available (call them from `tool` steps by name):"


def registry_planner_block(registry_schemas: list[dict]) -> str:
    """Prompt block advertising the registry tools (one line per tool).

    Mirrors `web_tools.planner_block` for the MCP registry. Returns ""
    for an empty list, so a failed or empty registry call leaves the
    planner prompt byte-identical to its pre-change form (spec:
    "Registry outage leaves planning unchanged").
    """
    lines: list[str] = []
    for schema in registry_schemas:
        if not isinstance(schema, dict):
            continue
        fn = schema.get("function", schema)
        if not isinstance(fn, dict):
            continue
        name = fn.get("name")
        if not isinstance(name, str) or not name:
            continue
        description = fn.get("description")
        desc = description.strip() if isinstance(description, str) else ""
        lines.append(f"- {name}: {desc}" if desc else f"- {name}")
    if not lines:
        return ""
    return "\n\n" + "\n".join([_REGISTRY_PLANNER_HEADER, *lines])
