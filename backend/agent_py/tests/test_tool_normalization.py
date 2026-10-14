"""MCP registry schema normalization, per-run memoization, dispatcher views,
and the planner catalog block — openspec/changes/agent-feed-tools tasks
2.1–2.4, 3.1–3.2.

Consumer-parser contract (design D8): normalization tests source their input
from the checked-in Go fixture `tests/fixtures/mcp_tools_list.json` (frozen by
`TestHandler_ToolsList_GoldenFixture` in internal/mcp) — never hand-written
OpenAI dicts, which is how the silent `inputSchema` drop hid before.
"""

from __future__ import annotations

import json
import logging
from pathlib import Path

import pytest

from agent.executor import _tool_schema_text
from agent.tool_dispatch import (
    LocalTool,
    LocalToolRegistry,
    ToolDispatcher,
    registry_planner_block,
)
from agent.tools_client import normalize_registry_tools

FIXTURE = Path(__file__).parent / "fixtures" / "mcp_tools_list.json"

FIVE_TOOLS = {"create_feed", "delete_feed", "get_feed", "list_feeds", "update_feed"}


@pytest.fixture
def captured_wire() -> list[dict]:
    """The raw `result.tools` array as the Go server serves it (native shape)."""
    return json.loads(FIXTURE.read_text())


class _StubMcp:
    def __init__(self, tools: list | None = None, fail: bool = False) -> None:
        self._tools = tools or []
        self._fail = fail
        self.list_calls = 0
        self.calls: list[tuple[str, dict]] = []

    async def list_tools(self):
        self.list_calls += 1
        if self._fail:
            raise RuntimeError("mcp down")
        return list(self._tools)

    async def call(self, name, arguments):
        self.calls.append((name, arguments))
        return {"ok": True}


def _local(name: str) -> LocalTool:
    async def _dispatch(args):
        return {}

    return LocalTool(
        name=name,
        schema={
            "type": "function",
            "function": {"name": name, "description": "", "parameters": {}},
        },
        dispatch=_dispatch,
    )


# --- 2.1 normalization from the captured wire --------------------------------


def test_normalization_binds_fixture_schemas(captured_wire):
    """Bound MCP tools carry their real argument schema (spec scenario),
    and annotations are preserved on the side map — not inside the bound
    function object."""
    schemas, annotations = normalize_registry_tools(captured_wire)

    by_name = {t["name"]: t for t in captured_wire}
    assert {s["function"]["name"] for s in schemas} == FIVE_TOOLS
    for schema in schemas:
        fn = schema["function"]
        assert schema["type"] == "function"
        assert fn["parameters"] == by_name[fn["name"]]["inputSchema"]
        assert "inputSchema" not in fn
        assert "annotations" not in fn  # never inside the bound object

    assert set(annotations) == FIVE_TOOLS
    assert annotations["list_feeds"]["readOnlyHint"] is True
    assert annotations["get_feed"]["readOnlyHint"] is True
    assert annotations["create_feed"]["readOnlyHint"] is False
    assert annotations["update_feed"]["readOnlyHint"] is False
    assert annotations["delete_feed"]["readOnlyHint"] is False
    assert annotations["create_feed"]["destructiveHint"] is True


def test_bound_list_feeds_parameters_are_non_empty(captured_wire):
    schemas, _ = normalize_registry_tools(captured_wire)
    list_feeds = next(s for s in schemas if s["function"]["name"] == "list_feeds")
    parameters = list_feeds["function"]["parameters"]
    assert parameters.get("type") == "object"
    assert parameters.get("properties")  # query / page / page_size / order


def test_unbindable_entries_skipped_not_fatal(caplog):
    raw = [
        {"description": "no name at all"},
        {"name": "bad_schema", "inputSchema": "not-an-object"},
        {"name": "ok_tool", "inputSchema": {"type": "object"}, "annotations": {}},
        "not-a-dict",
    ]
    with caplog.at_level(logging.WARNING, logger="nyx.agent.tools_client"):
        schemas, annotations = normalize_registry_tools(raw)

    assert [s["function"]["name"] for s in schemas] == ["ok_tool"]
    assert set(annotations) == {"ok_tool"}
    warnings = [r for r in caplog.records if r.levelno == logging.WARNING]
    assert len(warnings) >= 3  # each skippable entry logged, run not failed


def test_absent_annotations_do_not_crash(captured_wire):
    raw = [t for t in captured_wire if t["name"] == "create_feed"]
    raw[0] = {k: v for k, v in raw[0].items() if k != "annotations"}
    schemas, annotations = normalize_registry_tools(raw)
    assert schemas and annotations["create_feed"] == {}


# --- 2.2 retry re-prompt schema text ----------------------------------------


def test_retry_reprompt_schema_text_uses_normalized_shape(captured_wire):
    """Spec scenario "Retry re-prompt shows the real schema": the re-prompt
    text carries the tool's actual parameters — and the un-normalized wire
    shape is pinned as the trap this normalization fixes."""
    schemas, _ = normalize_registry_tools(captured_wire)
    text = _tool_schema_text(schemas, "list_feeds")
    assert text != "{}"
    parsed = json.loads(text)
    assert parsed.get("type") == "object"
    assert parsed.get("properties")

    # The trap pin: fed the raw MCP-native wire, the schema text is empty —
    # i.e. without 2.1 the re-prompt ships `{}` and this test would fail.
    assert _tool_schema_text(captured_wire, "list_feeds") == "{}"
    # Unknown tools still degrade to "{}" (the rejection text carries the error).
    assert _tool_schema_text(schemas, "nope") == "{}"


# --- 2.4 per-run memoization -------------------------------------------------


async def test_registry_fetch_is_memoized_per_run(captured_wire):
    mcp = _StubMcp(tools=captured_wire)
    dispatcher = ToolDispatcher(mcp=mcp, local=LocalToolRegistry())

    await dispatcher.list_tools()
    await dispatcher.list_tools()
    await dispatcher.registry_tools()
    assert await dispatcher.confirmation_required("list_feeds") is False
    assert await dispatcher.confirmation_required("create_feed") is True

    assert mcp.list_calls == 1  # one fetch, four consumers


async def test_failed_fetch_is_not_memoized():
    """A transient outage must not poison the rest of the run: the next
    consumer retries the fetch (task 2.4's deliberate non-memo of failure)."""
    mcp = _StubMcp(tools=[], fail=True)
    dispatcher = ToolDispatcher(mcp=mcp, local=LocalToolRegistry())
    assert await dispatcher.list_tools() == []
    assert mcp.list_calls == 1

    mcp._fail = False
    assert await dispatcher.list_tools() == []
    assert mcp.list_calls == 2


# --- 3.1 registry-only view --------------------------------------------------


async def test_registry_tools_excludes_local_names(captured_wire):
    local = LocalToolRegistry([_local("list_feeds")])  # local shadows a registry name
    dispatcher = ToolDispatcher(mcp=_StubMcp(tools=captured_wire), local=local)

    registry_view = await dispatcher.registry_tools()
    names = [s["function"]["name"] for s in registry_view]
    assert "list_feeds" not in names  # local wins; the MCP entry is hidden
    assert set(names) == FIVE_TOOLS - {"list_feeds"}


async def test_registry_tools_empty_on_failure():
    dispatcher = ToolDispatcher(mcp=_StubMcp(fail=True), local=LocalToolRegistry())
    assert await dispatcher.registry_tools() == []


# --- D3 confirmation policy --------------------------------------------------


async def test_confirmation_policy_fail_safe(captured_wire):
    by_name = {t["name"]: t for t in captured_wire}
    get_feed_no_annotations = {
        k: v for k, v in by_name["get_feed"].items() if k != "annotations"
    }
    stub_tools = [
        by_name["list_feeds"],
        by_name["create_feed"],
        by_name["delete_feed"],
        get_feed_no_annotations,
        {"name": "future_tool", "inputSchema": {"type": "object"}},
    ]
    local = LocalToolRegistry([_local("web_fetch")])
    dispatcher = ToolDispatcher(mcp=_StubMcp(tools=stub_tools), local=local)

    assert await dispatcher.confirmation_required("list_feeds") is False
    assert await dispatcher.confirmation_required("create_feed") is True
    assert await dispatcher.confirmation_required("delete_feed") is True
    # In-view tool with absent annotations gates (fail-safe, D3).
    assert await dispatcher.confirmation_required("get_feed") is True
    assert await dispatcher.confirmation_required("future_tool") is True
    # Not a registry tool / agent-native: never gated.
    assert await dispatcher.confirmation_required("no_such_tool") is False
    assert await dispatcher.confirmation_required("web_fetch") is False


# --- 3.2 planner block builder ----------------------------------------------


async def test_registry_planner_block_lists_captured_tools(captured_wire):
    dispatcher = ToolDispatcher(mcp=_StubMcp(tools=captured_wire), local=LocalToolRegistry())
    block = registry_planner_block(await dispatcher.registry_tools())
    assert block.startswith("\n\nRegistry tools available")
    for name in FIVE_TOOLS:
        assert f"- {name}" in block


async def test_registry_planner_block_empty_when_no_tools():
    """Registry outage leaves planning unchanged: empty list → "" — the
    planner prompt is then byte-identical to its pre-change form."""
    assert registry_planner_block([]) == ""
    dispatcher = ToolDispatcher(mcp=_StubMcp(fail=True), local=LocalToolRegistry())
    assert registry_planner_block(await dispatcher.registry_tools()) == ""
