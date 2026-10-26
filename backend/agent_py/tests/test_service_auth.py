"""Tests for the inbound service-token gate."""

from __future__ import annotations

import httpx
import pytest

from agent.config import Settings
from agent.main import create_app
from agent.service_auth import SERVICE_TOKEN_HEADER, ServiceTokenMiddleware
from tests.service_auth_fixtures import SERVICE_AUTH_HEADERS, TEST_SERVICE_TOKEN


def _app(*, requests_per_second: float = 100.0, burst: int = 10):
    settings = Settings(
        _env_file=None,
        llm_base_url="http://vllm:8000/v1",
        service_token=TEST_SERVICE_TOKEN,
        requests_per_second=requests_per_second,
        burst=burst,
    )
    return create_app(settings)


async def _request(app, method: str, path: str, headers=None):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        return await client.request(method, path, headers=headers, json={"messages": []})


@pytest.mark.parametrize(
    "headers",
    [{}, {"X-Nyx-Service-Token": "wrong-token-0123456789-0123456789"}],
)
async def test_missing_or_wrong_token_rejected(headers):
    resp = await _request(_app(), "POST", "/agent/run", headers=headers)
    assert resp.status_code == 401
    assert resp.json() == {"detail": "unauthorized"}


async def test_non_ascii_token_rejected_cleanly():
    resp = await _request(
        _app(),
        "POST",
        "/agent/run",
        headers={"X-Nyx-Service-Token": "café-token-0123456789-0123456789".encode("latin-1")},
    )
    assert resp.status_code == 401
    assert resp.json() == {"detail": "unauthorized"}


async def test_health_probe_stays_open():
    resp = await _request(_app(), "GET", "/healthz")
    assert resp.status_code in (200, 503)


async def test_health_path_must_match_exactly():
    resp = await _request(_app(), "GET", "/healthz/")
    assert resp.status_code == 401


async def test_metrics_on_app_port_is_gated():
    resp = await _request(_app(), "GET", "/metrics")
    assert resp.status_code == 401


async def test_metrics_with_valid_token_exposed():
    resp = await _request(_app(), "GET", "/metrics", headers=SERVICE_AUTH_HEADERS)
    assert resp.status_code == 200


async def test_valid_token_reaches_handler():
    resp = await _request(_app(), "POST", "/agent/run", headers=SERVICE_AUTH_HEADERS)
    assert resp.status_code == 400
    assert "X-Nyx-User-Id" in resp.json()["detail"]


async def test_token_gate_runs_before_rate_limit():
    app = _app(requests_per_second=0.001, burst=1)
    for _ in range(5):
        resp = await _request(
            app,
            "POST",
            "/agent/run",
            headers={"X-Nyx-Service-Token": "wrong-token-0123456789-0123456789"},
        )
        assert resp.status_code == 401
    resp = await _request(app, "POST", "/agent/run", headers=SERVICE_AUTH_HEADERS)
    assert resp.status_code != 429


def test_service_token_header_literal():
    assert SERVICE_TOKEN_HEADER == "x-nyx-service-token"  # noqa: S105


def test_middleware_rejects_short_token():
    async def _app_stub(scope, receive, send):
        return None

    with pytest.raises(ValueError):
        ServiceTokenMiddleware(_app_stub, expected_token="short")
