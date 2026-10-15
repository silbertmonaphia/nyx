"""HTTP client for the Go RAG bridge endpoint (`/internal/rag/retrieve`).

The Go side is mounted as `r.Get("/internal/rag/retrieve", ...)` and reads
`r.URL.Query().Get("query")` — GET + query param, NOT POST + JSON body.
The Go side reads the user identifier from the verified JWT (NOT from the request),
so we don't pass `user_id` at all. The Go bridge returns
`{"context_block": str, "citations": [{"feed_id": int, "snippet": str}]}` on 200,
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
    def __init__(self, base_url: str, bearer_token: str) -> None:
        self._base_url = base_url.rstrip("/")
        self._bearer_token = bearer_token

    async def retrieve(self, query: str) -> RagResult:
        # The Go bridge is a GET + query-param endpoint. We pass the bearer
        # token as Authorization and the query string as `?query=`. The Go
        # side reads the user identifier from the JWT — there's no user_id
        # param. If the body is JSON, chi will reject it (405 Method Not
        # Allowed), so we MUST stay on GET.
        try:
            async with httpx.AsyncClient(timeout=30.0) as client:
                resp = await client.get(
                    self._base_url,
                    params={"query": query},
                    headers={"Authorization": f"Bearer {self._bearer_token}"},
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

        # Tolerate missing fields; treat as empty.
        return RagResult(
            context_block=str(data.get("context_block", "")),
            citations=list(data.get("citations", [])),
        )