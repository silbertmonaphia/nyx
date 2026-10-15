"""RAG client tests — httpx MockTransport for canned responses."""

from __future__ import annotations

import httpx
import pytest

from agent.rag_client import RagAuthError, RagClient, RagUnavailableError


def _patched_async_client(handler, original):
    """Returns a factory that builds an httpx.AsyncClient with MockTransport, calling `original` (the real AsyncClient) so monkeypatch doesn't recurse."""
    def _factory(*args, **kwargs):
        return original(transport=httpx.MockTransport(handler), *args, **kwargs)
    return _factory


@pytest.mark.asyncio
async def test_retrieve_happy_path(monkeypatch):
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["auth"] = request.headers.get("authorization")
        captured["url_path"] = request.url.path
        captured["url_query"] = request.url.params
        captured["method"] = request.method
        return httpx.Response(
            200,
            json={"context_block": "feeds: 1, 2, 3", "citations": [{"feed_id": 1, "snippet": "x"}]},
        )

    monkeypatch.setattr(httpx, "AsyncClient", _patched_async_client(handler, httpx.AsyncClient))
    client = RagClient("http://rag.test", "test-token")
    result = await client.retrieve("what are my feeds?")

    assert result.context_block == "feeds: 1, 2, 3"
    assert result.citations == [{"feed_id": 1, "snippet": "x"}]
    assert captured["auth"] == "Bearer test-token"
    assert captured["method"] == "GET", "RAG bridge must be called via GET, not POST"
    assert captured["url_path"] == "/", "base URL path is preserved (test stub uses '/')"
    assert captured["url_query"].get("query") == "what are my feeds?"


@pytest.mark.asyncio
async def test_retrieve_401_raises_auth_error(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(401, text="unauthorized")

    monkeypatch.setattr(httpx, "AsyncClient", _patched_async_client(handler, httpx.AsyncClient))
    client = RagClient("http://rag.test", "bad-token")
    with pytest.raises(RagAuthError):
        await client.retrieve("q")


@pytest.mark.asyncio
async def test_retrieve_500_raises_unavailable(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(500, text="server error")

    monkeypatch.setattr(httpx, "AsyncClient", _patched_async_client(handler, httpx.AsyncClient))
    client = RagClient("http://rag.test", "test-token")
    with pytest.raises(RagUnavailableError):
        await client.retrieve("q")