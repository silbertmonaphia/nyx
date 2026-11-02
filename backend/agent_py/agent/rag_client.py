"""HTTP client for the Go RAG bridge endpoint (`/internal/rag/retrieve`).

The Go side is mounted as `r.Get("/internal/rag/retrieve", ...)` and reads
`r.URL.Query().Get("query")` — GET + query param, NOT POST + JSON body.
The Go side reads the user identifier from the verified JWT (NOT from the request),
so we don't pass `user_id` at all. The Go bridge returns
`{"context_block": str, "citations": [...]}` on 200,
`{"error": str, ...}` on 4xx/5xx via the standard error envelope.
"""

from __future__ import annotations

from typing import NamedTuple

import httpx


class RagAuthError(Exception):
    """401 from the RAG bridge — token rejected."""


class RagUnavailableError(Exception):
    """Connect refused, timeout, or 5xx from the RAG bridge."""


class RagResult(NamedTuple):
    context_block: str
    citations: list[dict]


class RagClient:
    def __init__(self, base_url: str, bearer_token: str, transport: httpx.AsyncBaseTransport | None = None) -> None:
        self._base_url = base_url.rstrip("/")
        self._bearer_token = bearer_token
        self._transport = transport
        if self._base_url.endswith("/retrieve"):
            self._outline_url = self._base_url[: -len("/retrieve")] + "/outline"
        else:
            self._outline_url = self._base_url + "/outline"

    def _headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self._bearer_token}"}

    async def retrieve(
        self,
        query: str,
        corpus: str = "feeds",
        source: int | None = None,
        path: str | None = None,
        top_k: int | None = None,
    ) -> RagResult:
        params: dict[str, str] = {"query": query}
        if corpus and corpus != "feeds":
            params["corpus"] = corpus
        if source is not None:
            params["source"] = str(int(source))
        if path is not None:
            params["path"] = path
        if top_k is not None:
            params["top_k"] = str(int(top_k))
        try:
            async with httpx.AsyncClient(timeout=30.0, transport=self._transport) as client:
                resp = await client.get(
                    self._base_url,
                    params=params,
                    headers=self._headers(),
                )
        except (httpx.ConnectError, httpx.TimeoutException, httpx.RequestError) as exc:
            raise RagUnavailableError(repr(exc)) from exc

        if resp.status_code == 401:
            raise RagAuthError("RAG bridge rejected the bearer token")
        if resp.status_code >= 500:
            raise RagUnavailableError(f"RAG bridge {resp.status_code}: {resp.text[:200]}")
        if resp.status_code != 200:
            raise RagUnavailableError(f"RAG bridge {resp.status_code}: {resp.text[:200]}")

        try:
            data = resp.json()
        except ValueError as exc:
            raise RagUnavailableError(f"RAG bridge returned non-JSON: {exc}") from exc

        return RagResult(
            context_block=str(data.get("context_block", "")),
            citations=list(data.get("citations", [])),
        )

    async def outline(self) -> list[dict]:
        try:
            async with httpx.AsyncClient(timeout=2.0, transport=self._transport) as client:
                resp = await client.get(self._outline_url, headers=self._headers())
        except (httpx.ConnectError, httpx.TimeoutException, httpx.RequestError) as exc:
            raise RagUnavailableError(repr(exc)) from exc
        if resp.status_code == 401:
            raise RagAuthError("RAG bridge rejected the bearer token")
        if resp.status_code != 200:
            raise RagUnavailableError(f"RAG bridge {resp.status_code}: {resp.text[:200]}")
        try:
            data = resp.json()
        except ValueError as exc:
            raise RagUnavailableError(f"RAG bridge returned non-JSON: {exc}") from exc
        sources = data.get("sources", [])
        if not isinstance(sources, list):
            return []
        return [source for source in sources if isinstance(source, dict)]
