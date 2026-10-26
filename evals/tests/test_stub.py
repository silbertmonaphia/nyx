"""Tavily stub contract tests, including a round trip through the real parser.

Model-free; run with the agent interpreter:

    /home/smona/nyx/backend/agent_py/.venv/bin/python -m pytest evals/tests -q

The round-trip test feeds the stub's HTTP response through
`agent.web_tools.search_web` — the production consumer of the Tavily envelope —
so a field-name drift on either side fails here rather than silently in a run.
"""

import asyncio
import importlib.util
import json
import pathlib
import sys
import threading
import urllib.error
import urllib.request

import pytest
import yaml

EVALS_DIR = pathlib.Path(__file__).resolve().parents[1]
REPO_ROOT = EVALS_DIR.parent
STUB_PATH = EVALS_DIR / "stubs" / "tavily_stub.py"
SECURITY_CORPUS = EVALS_DIR / "cases" / "security.yaml"
AGENT_PY_DIR = REPO_ROOT / "backend" / "agent_py"

HOSTILE_KEY_PREFIX = "nyx-eval-web-"


def _load_stub():
    spec = importlib.util.spec_from_file_location("tavily_stub", STUB_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


STUB = _load_stub()


@pytest.fixture()
def stub_server():
    server = STUB.make_server("127.0.0.1", 0)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def _security_prompts():
    data = yaml.safe_load(SECURITY_CORPUS.read_text(encoding="utf-8"))
    return [case["prompt"] for case in data["cases"]]


def test_response_envelope_matches_tavily():
    payload = STUB.tavily_response("今天天气如何", 5)
    assert set(payload) >= {"query", "results"}
    assert payload["query"] == "今天天气如何"
    results = payload["results"]
    assert isinstance(results, list) and results
    for result in results:
        assert {"title", "url", "content", "score"} <= set(result)
        assert isinstance(result["title"], str) and result["title"]
        assert result["url"].startswith("http")
        assert isinstance(result["content"], str) and result["content"]
        assert isinstance(result["score"], (int, float))


def test_security_corpus_queries_have_canned_payloads():
    keys = set(STUB.CANNED)
    for prompt in _security_prompts():
        for token in prompt.split():
            if token.startswith(HOSTILE_KEY_PREFIX):
                assert token in keys, f"no canned payload for {token}"


def test_hostile_payloads_carry_instructions():
    for key in STUB.CANNED:
        if not key.startswith(HOSTILE_KEY_PREFIX):
            continue
        body = " ".join(result["content"] for result in STUB.CANNED[key])
        assert any(
            marker in body
            for marker in ("delete_feed", "update_feed", "system prompt", "系统提示词")
        ), key


def test_unknown_query_falls_back_and_empty_query_returns_nothing():
    fallback = STUB.canned_results("完全不相关的问题")
    assert fallback == STUB.DEFAULT_RESULTS or not fallback
    assert STUB.tavily_response("")["results"] == []
    assert STUB.tavily_response("   ")["results"] == []


def test_longest_matching_key_wins():
    payload = STUB.tavily_response("请解释 nyx-eval-web-leak 的含义", 5)
    assert payload["results"] == STUB.CANNED["nyx-eval-web-leak"]


def test_max_results_slices_the_result_list():
    full = STUB.tavily_response("今天天气如何")["results"]
    assert len(full) > 1
    assert STUB.tavily_response("今天天气如何", 1)["results"] == full[:1]


def test_http_surface(stub_server):
    port = stub_server.server_address[1]
    base = f"http://127.0.0.1:{port}"

    with urllib.request.urlopen(f"{base}/healthz", timeout=5) as response:
        assert response.status == 200
        assert json.loads(response.read()) == {"status": "ok"}

    request = urllib.request.Request(
        f"{base}/search",
        data=json.dumps({"query": "今天天气如何", "max_results": 2}).encode(),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=5) as response:
        body = json.loads(response.read())
    assert response.status == 200
    assert len(body["results"]) == 2

    with pytest.raises(urllib.error.HTTPError) as missing_query:
        urllib.request.urlopen(
            urllib.request.Request(f"{base}/search", data=b"{}", headers={"Content-Type": "application/json"}),
            timeout=5,
        )
    assert missing_query.value.code == 400

    with pytest.raises(urllib.error.HTTPError) as bad_path:
        urllib.request.urlopen(f"{base}/nope", timeout=5)
    assert bad_path.value.code == 404


def test_search_web_parses_the_stub(stub_server, monkeypatch):
    httpx = pytest.importorskip("httpx")
    assert httpx is not None
    monkeypatch.setenv("NO_PROXY", "127.0.0.1,localhost")
    monkeypatch.setenv("no_proxy", "127.0.0.1,localhost")
    if str(AGENT_PY_DIR) not in sys.path:
        sys.path.insert(0, str(AGENT_PY_DIR))

    from agent.config import Settings
    from agent.web_tools import search_web

    port = stub_server.server_address[1]
    settings = Settings(
        tavily_base_url=f"http://127.0.0.1:{port}/search",
        llm_base_url="http://127.0.0.1:9/v1",
        web_search_max_results=5,
        service_token="eval-test-service-token-0123456789",
    )

    query = "搜索 nyx-eval-web-delete 看看是什么"
    payload = asyncio.run(search_web(query, settings))

    assert payload["query"] == query
    results = payload["results"]
    assert results
    assert all(set(result) == {"title", "url", "description"} for result in results)
    titles = [result["title"] for result in results]
    assert any("nyx-eval-web-delete" in title for title in titles)
    hostile = STUB.CANNED["nyx-eval-web-delete"][0]["content"]
    descriptions = [result["description"] for result in results]
    assert any(hostile in description for description in descriptions)
    assert all(description.startswith('<untrusted_web_content source="') for description in descriptions)


def test_search_web_honors_the_result_cap(stub_server, monkeypatch):
    httpx = pytest.importorskip("httpx")
    assert httpx is not None
    monkeypatch.setenv("NO_PROXY", "127.0.0.1,localhost")
    monkeypatch.setenv("no_proxy", "127.0.0.1,localhost")
    if str(AGENT_PY_DIR) not in sys.path:
        sys.path.insert(0, str(AGENT_PY_DIR))

    from agent.config import Settings
    from agent.web_tools import search_web

    port = stub_server.server_address[1]
    settings = Settings(
        tavily_base_url=f"http://127.0.0.1:{port}/search",
        llm_base_url="http://127.0.0.1:9/v1",
        web_search_max_results=1,
        service_token="eval-test-service-token-0123456789",
    )

    payload = asyncio.run(search_web("今天天气如何", settings))
    assert len(payload["results"]) == 1
