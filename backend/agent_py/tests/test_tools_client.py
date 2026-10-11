"""Tools (MCP) client tests — httpx MockTransport for canned JSON-RPC responses."""

from __future__ import annotations

import json

import httpx
import pytest

from agent.tools_client import ToolAuthError, ToolError, ToolUnavailableError, ToolsClient


def _patched_async_client(handler, original):
    def _factory(*args, **kwargs):
        return original(transport=httpx.MockTransport(handler), *args, **kwargs)
    return _factory


@pytest.mark.asyncio
async def test_call_happy_path(monkeypatch):
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["auth"] = request.headers.get("authorization")
        body = json.loads(request.content)
        captured["body"] = body
        assert body["jsonrpc"] == "2.0"
        assert body["method"] == "tools/call"
        assert body["params"] == {"name": "list_feeds", "arguments": {"limit": 5}}
        return httpx.Response(200, json={"jsonrpc": "2.0", "id": body["id"], "result": {"items": [1, 2]}})

    monkeypatch.setattr(httpx, "AsyncClient", _patched_async_client(handler, httpx.AsyncClient))
    client = ToolsClient("http://mcp.test", "test-token")
    result = await client.call("list_feeds", {"limit": 5})

    assert result == {"items": [1, 2]}
    assert captured["auth"] == "Bearer test-token"


@pytest.mark.asyncio
async def test_call_401_raises_auth_error(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(401, text="unauthorized")

    monkeypatch.setattr(httpx, "AsyncClient", _patched_async_client(handler, httpx.AsyncClient))
    client = ToolsClient("http://mcp.test", "bad-token")
    with pytest.raises(ToolAuthError):
        await client.call("list_feeds", {})


@pytest.mark.asyncio
async def test_call_500_raises_unavailable(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(500, text="server error")

    monkeypatch.setattr(httpx, "AsyncClient", _patched_async_client(handler, httpx.AsyncClient))
    client = ToolsClient("http://mcp.test", "test-token")
    with pytest.raises(ToolUnavailableError):
        await client.call("list_feeds", {})


@pytest.mark.asyncio
async def test_call_jsonrpc_error_raises_tool_error(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "jsonrpc": "2.0",
                "id": "abc",
                "error": {"code": -32601, "message": "tool not found"},
            },
        )

    monkeypatch.setattr(httpx, "AsyncClient", _patched_async_client(handler, httpx.AsyncClient))
    client = ToolsClient("http://mcp.test", "test-token")
    with pytest.raises(ToolError) as ei:
        await client.call("missing_tool", {})
    assert "tool not found" in str(ei.value)


@pytest.mark.asyncio
async def test_list_tools_returns_openai_schemas(monkeypatch):
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["body"] = json.loads(request.content)
        return httpx.Response(
            200,
            json={
                "jsonrpc": "2.0",
                "id": captured["body"]["id"],
                "result": {
                    "tools": [
                        {
                            "type": "function",
                            "function": {
                                "name": "list_feeds",
                                "description": "List user's feeds",
                                "parameters": {"type": "object", "properties": {}},
                            },
                        }
                    ]
                },
            },
        )

    monkeypatch.setattr(httpx, "AsyncClient", _patched_async_client(handler, httpx.AsyncClient))
    client = ToolsClient("http://mcp.test", "test-token")
    schemas = await client.list_tools()

    assert len(schemas) == 1
    assert schemas[0]["function"]["name"] == "list_feeds"
    # And confirm the request was a JSON-RPC `tools/list` call.
    assert captured["body"]["method"] == "tools/list"