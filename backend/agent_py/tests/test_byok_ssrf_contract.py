"""Cross-runtime BYOK SSRF contract: the shared fixture at
tests/fixtures/ssrf_denied_urls.json lists URLs that BOTH the Go
save-time validator (backend/internal/llmsettings) and this agent's
search guard must refuse. This module is the Python consumer; it runs
bare (no TEST_ASYNC_PG_DSN) and performs no live DNS — every fixture
entry is an IP literal the net_guard classifies locally."""

import json
from pathlib import Path

import httpx
import pytest

from agent.tools_client import ToolError
from agent.types import TavilyOverride
from agent.web_tools import search_web
from tests.test_web_tools import make_settings, patched_async_client

FIXTURE = Path(__file__).parent / "fixtures" / "ssrf_denied_urls.json"


def fixture_cases():
    data = json.loads(FIXTURE.read_text())
    return data["cases"]


@pytest.mark.parametrize("case", fixture_cases(), ids=lambda c: c["url"])
async def test_search_refuses_shared_denied_urls(monkeypatch, case):
    def handler(request: httpx.Request) -> httpx.Response:
        raise AssertionError(f"must not dial {case['url']}")

    patched_async_client(monkeypatch, handler)
    settings = make_settings(web_search_enabled=True)
    override = TavilyOverride(base_url=case["url"], api_key="user-key")

    with pytest.raises(ToolError) as excinfo:
        await search_web("q", settings, tavily_override=override)
    message = str(excinfo.value)
    assert "non-public address" in message, message
    assert "ports 80/443" not in message, message
