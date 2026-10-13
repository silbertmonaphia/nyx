"""HTTP client for the Go MCP `tools/call` JSON-RPC endpoint.

The MCP shim in `internal/mcp/handler.go` speaks JSON-RPC 2.0 over HTTP. We send
`{"jsonrpc":"2.0","method":"tools/call","params":{"name":..., "arguments":{...}},"id":<uuid>}`
and receive `{"result": ...}` on success or `{"error": {...}}` on failure.
"""

from __future__ import annotations

import uuid

import httpx


class ToolAuthError(Exception):
    """401 from the MCP server — token rejected."""


class ToolUnavailableError(Exception):
    """Connect refused, timeout, 5xx, 408, or 429 from the MCP server."""


class ToolError(Exception):
    """JSON-RPC `error` response — semantic rejection (tool-not-found, validation, etc.)."""


class ToolRejectedError(Exception):
    """HTTP 4xx (other than 401/408/429) with no JSON-RPC error envelope.

    Non-retryable: the request shape was rejected at the transport level and
    retrying the same call cannot succeed. The executor fails the step
    immediately (see agent-tool-dispatch-retry §4.4). `status_code` carries
    the HTTP status for safe, non-leaking error text.
    """

    def __init__(self, message: str, status_code: int | None = None) -> None:
        super().__init__(message)
        self.status_code = status_code


class ToolsClient:
    def __init__(self, base_url: str, bearer_token: str) -> None:
        self._base_url = base_url.rstrip("/")
        self._bearer_token = bearer_token

    async def list_tools(self) -> list[dict]:
        """Call MCP `tools/list` and return the OpenAI-format tool schemas.

        Each entry is `{type: "function", function: {name, description, parameters}}`
        which LangChain's `ChatOpenAI.bind_tools(...)` accepts directly.

        Used by the executor to bind tool definitions on the react chat client
        so the model can actually emit tool_calls (HIGH-7 fix — without this the
        react loop is effectively dead because the model has no tools to call).
        """
        body = {
            "jsonrpc": "2.0",
            "method": "tools/list",
            "params": {},
            "id": str(uuid.uuid4()),
        }
        try:
            async with httpx.AsyncClient(timeout=15.0) as client:
                resp = await client.post(
                    self._base_url,
                    json=body,
                    headers={
                        "Authorization": f"Bearer {self._bearer_token}",
                        "Content-Type": "application/json",
                    },
                )
        except (httpx.ConnectError, httpx.TimeoutException, httpx.RequestError) as exc:
            raise ToolUnavailableError(repr(exc)) from exc

        if resp.status_code == 401:
            raise ToolAuthError("MCP rejected the bearer token (tools/list)")
        if resp.status_code >= 500:
            raise ToolUnavailableError(f"MCP {resp.status_code} on tools/list")
        if resp.status_code != 200:
            raise ToolError(f"MCP {resp.status_code}: {resp.text[:200]}")

        try:
            data = resp.json()
        except ValueError as exc:
            raise ToolUnavailableError(f"MCP returned non-JSON: {exc}") from exc

        if "error" in data and data["error"] is not None:
            err = data["error"]
            msg = err.get("message", "tools/list error") if isinstance(err, dict) else str(err)
            raise ToolError(f"tools/list: {msg}")

        result = data.get("result", {})
        # MCP `tools/list` returns `{"tools": [...]}`. The list items may already
        # be in OpenAI format, or they may be MCP-native. Pass through anything
        # that has a `type: function` envelope; pass through the raw list as
        # a fallback for MCP-native shapes that LangChain rejects.
        tools = result.get("tools", result) if isinstance(result, dict) else result
        return tools if isinstance(tools, list) else []

    async def call(self, name: str, arguments: dict) -> dict:
        body = {
            "jsonrpc": "2.0",
            "method": "tools/call",
            "params": {"name": name, "arguments": arguments},
            "id": str(uuid.uuid4()),
        }
        try:
            async with httpx.AsyncClient(timeout=30.0) as client:
                resp = await client.post(
                    self._base_url,
                    json=body,
                    headers={
                        "Authorization": f"Bearer {self._bearer_token}",
                        "Content-Type": "application/json",
                    },
                )
        except (httpx.ConnectError, httpx.TimeoutException, httpx.RequestError) as exc:
            raise ToolUnavailableError(repr(exc)) from exc

        if resp.status_code == 401:
            raise ToolAuthError("MCP rejected the bearer token")
        if resp.status_code >= 500:
            raise ToolUnavailableError(f"MCP {resp.status_code}: {resp.text[:200]}")
        if resp.status_code in (408, 429):
            # Transient throttling / timeout — retryable with backoff.
            raise ToolUnavailableError(f"MCP {resp.status_code}: retryable transport status")
        if resp.status_code != 200:
            # 4xx (other than 401/408/429). Status alone does not decide
            # retryability: a body carrying a JSON-RPC error envelope is a
            # semantic (registry) rejection and routes to the tool-arg
            # retry path; a bare 4xx is a non-retryable transport rejection.
            try:
                data = resp.json()
            except ValueError:
                data = None
            if isinstance(data, dict) and data.get("error") is not None:
                err = data["error"]
                msg = err.get("message", "tool error") if isinstance(err, dict) else str(err)
                raise ToolError(f"{name}: {msg}")
            raise ToolRejectedError(
                f"MCP {resp.status_code}: {resp.text[:200]}", status_code=resp.status_code
            )

        try:
            data = resp.json()
        except ValueError as exc:
            raise ToolUnavailableError(f"MCP returned non-JSON: {exc}") from exc

        if "error" in data and data["error"] is not None:
            err = data["error"]
            msg = err.get("message", "tool error") if isinstance(err, dict) else str(err)
            raise ToolError(f"{name}: {msg}")
        return data.get("result", {})