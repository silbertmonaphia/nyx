import httpx
import pytest

from agent.rag_client import RagAuthError, RagClient, RagUnavailableError


def make_client(handler) -> RagClient:
    return RagClient(
        "http://test/internal/rag/retrieve",
        "token",
        transport=httpx.MockTransport(handler),
    )


@pytest.mark.asyncio
async def test_feed_shape_keeps_legacy_params():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["params"] = dict(request.url.params)
        return httpx.Response(
            200,
            json={
                "context_block": "feed context",
                "citations": [{"kind": "feed", "feed_id": 1, "snippet": "snippet"}],
            },
        )

    result = await make_client(handler).retrieve("hello")
    assert result.context_block == "feed context"
    assert result.citations[0]["kind"] == "feed"
    assert result.citations[0]["feed_id"] == 1
    assert "corpus" not in seen["params"]


@pytest.mark.asyncio
async def test_knowledge_shape_and_narrowing_params():
    def handler(request: httpx.Request) -> httpx.Response:
        params = request.url.params
        assert params["corpus"] == "kb"
        assert params["source"] == "3"
        assert params["path"] == "B-Sport/"
        assert params["top_k"] == "7"
        return httpx.Response(
            200,
            json={
                "context_block": "doc context",
                "citations": [
                    {
                        "kind": "doc",
                        "feed_id": 0,
                        "doc_id": 9,
                        "source_name": "shared",
                        "rel_path": "B-Sport/bjj.md",
                        "heading_path": ["Sport", "BJJ"],
                        "locator": {"line": 12},
                        "snippet": "chunk body",
                    }
                ],
            },
        )

    result = await make_client(handler).retrieve(
        "query", corpus="kb", source=3, path="B-Sport/", top_k=7
    )
    citation = result.citations[0]
    assert citation["kind"] == "doc"
    assert citation["feed_id"] == 0
    assert citation["doc_id"] == 9
    assert citation["source_name"] == "shared"
    assert citation["rel_path"] == "B-Sport/bjj.md"
    assert citation["heading_path"] == ["Sport", "BJJ"]
    assert citation["locator"] == {"line": 12}
    assert citation["snippet"] == "chunk body"


@pytest.mark.asyncio
async def test_unknown_corpus_400_is_unavailable():
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(400, json={"error": "Invalid corpus parameter"})

    with pytest.raises(RagUnavailableError):
        await make_client(handler).retrieve("hello", corpus="documents")


@pytest.mark.asyncio
async def test_auth_failure_raises_auth_error():
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(401, json={"error": "Authentication required"})

    with pytest.raises(RagAuthError):
        await make_client(handler).retrieve("hello")


@pytest.mark.asyncio
async def test_connect_error_is_unavailable():
    def handler(_: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("refused")

    with pytest.raises(RagUnavailableError):
        await make_client(handler).retrieve("hello")


@pytest.mark.asyncio
async def test_outline_reads_sources():
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/internal/rag/outline"
        return httpx.Response(
            200,
            json={
                "sources": [
                    {
                        "id": 1,
                        "name": "shared",
                        "scope": "shared",
                        "document_count": 3,
                        "chunk_count": 9,
                        "directories": ["B-Cook", "B-Sport"],
                    }
                ]
            },
        )

    sources = await make_client(handler).outline()
    assert sources[0]["name"] == "shared"
    assert sources[0]["directories"] == ["B-Cook", "B-Sport"]
