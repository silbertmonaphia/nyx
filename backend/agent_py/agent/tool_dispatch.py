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

Local tools raise the same exception types the MCP client raises
(`ToolError` / `ToolUnavailableError` / `ToolRejectedError`), so
`executor._classify_tool_failure` works unchanged (design.md D2).
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any

from .tools_client import ToolsClient

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
    the collision warning "once per run".
    """

    def __init__(self, mcp: ToolsClient, local: LocalToolRegistry | None = None) -> None:
        self._mcp = mcp
        self._local = local if local is not None else LocalToolRegistry()
        self._collisions_warned: set[str] = set()

    async def list_tools(self) -> list[dict]:
        local_schemas = self._local.schemas()
        local_names = {_schema_name(s) for s in local_schemas}

        try:
            mcp_schemas = await self._mcp.list_tools()
        except Exception as exc:  # noqa: BLE001 — degrade, never fail the list
            if local_schemas:
                log.warning(
                    "MCP tools/list failed; serving agent-native tools only: %r", exc
                )
            else:
                log.warning("MCP tools/list failed: %r", exc)
            return local_schemas

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

    async def call(self, name: str, arguments: dict) -> dict:
        if name in self._local:
            return await self._local.get(name).dispatch(arguments)
        return await self._mcp.call(name, arguments)
