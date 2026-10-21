"""Deterministic tests for the golden-set provider.

Two kinds of input, deliberately kept apart:

- the wire-parsing tests read the REAL captures under
  `evals/tests/fixtures/` (one live run per shape, recorded by
  `evals/tools/record_fixtures.py`) — the provider is the consumer of
  those exact bytes, so a producer/consumer drift shows up here;
- the orchestration tests drive the provider with an in-process stub
  transport, because dialogue run-id handling, approval timing, prefs
  polling and preflight failures cannot be provoked live at will.

No test here needs the model, the network, or Postgres.
"""
from __future__ import annotations

import json
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "providers"))

import agent_run  # noqa: E402

FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"
FIXED_CLOCK = lambda: datetime(2026, 1, 1, 0, 0, tzinfo=timezone.utc)  # noqa: E731


def sse_text(*frames, comment=None, sentinel=True) -> str:
    blocks = []
    if comment:
        blocks.append(f": {comment}")
    for event, data in frames:
        blocks.append(f"event: {event}\ndata: {json.dumps(data)}")
    if sentinel:
        blocks.append("data: [DONE]")
    return "\n\n".join(blocks) + "\n\n"


def auth_payload(user_id: int = 7) -> dict:
    return {
        "access_token": "test-access-token",
        "refresh_token": "test-refresh-token",
        "token_type": "Bearer",
        "expires_at": "2030-01-01T00:00:00Z",
        "user": {"id": user_id},
    }


class FakeClock:
    def __init__(self) -> None:
        self.now = 0.0

    def monotonic(self) -> float:
        return self.now

    def sleep(self, seconds: float) -> None:
        self.now += seconds


class StubTransport:
    """Records an ordered call log; replays canned SSE through the real parser."""

    def __init__(
        self,
        *,
        sse: str = "",
        json_responses: dict | None = None,
        existing_feeds: list | None = None,
        feeds_pages: list | None = None,
        status_code: int = 200,
        log: list | None = None,
    ) -> None:
        self.sse = sse
        self.json_responses = dict(json_responses or {})
        self.existing_feeds = list(existing_feeds or [])
        self.feeds_pages = list(feeds_pages) if feeds_pages is not None else None
        self.status_code = status_code
        self.log: list[tuple] = log if log is not None else []

    def status(self, path: str, *, timeout: float) -> int:
        self.log.append(("status", path))
        return self.status_code

    def get_json(self, path: str, *, headers: dict, timeout: float) -> dict:
        self.log.append(("get", path))
        if path.startswith("/api/feeds"):
            if self.feeds_pages is not None:
                assert self.feeds_pages, f"unexpected extra feeds GET: {path}"
                return self.feeds_pages.pop(0)
            return {"data": [{"title": t} for t in self.existing_feeds]}
        return self.json_responses.get(path, {})

    def post_json(self, path: str, *, headers: dict, body: dict, timeout: float) -> dict:
        self.log.append(("post", path, dict(body), dict(headers)))
        return self.json_responses.get(path, {})

    def stream_sse(self, path: str, *, headers: dict, body: dict, timeout: float, on_frame):
        self.log.append(("stream", path, dict(body), dict(headers)))
        for block in agent_run.iter_sse_blocks(self.sse.splitlines()):
            frame = agent_run.parse_sse_block(block)
            if frame is None:
                continue
            self.log.append(("frame", frame.event))
            on_frame(frame)
        return self.sse


class UnreachableTransport(StubTransport):
    def status(self, path: str, *, timeout: float) -> int:
        raise agent_run.ProviderError(f"GET {path} failed: ConnectError")


class ScriptedPrefs:
    def __init__(self, results: list[bool]) -> None:
        self.results = list(results)
        self.calls: list[tuple] = []

    def has_row(self, user_id: int, key: str | None) -> bool:
        self.calls.append((user_id, key))
        if not self.results:
            return False
        if len(self.results) == 1:
            return self.results[0]
        return self.results.pop(0)


def make_config(tmp_path: Path, **overrides) -> agent_run.Config:
    values = {
        "go_api_url": "http://go.test",
        "agent_url": "http://agent.test",
        "artifacts_dir": tmp_path,
        "run_stamp": "20260101T000000Z",
        "db_dsn": "postgresql://unused",
        "web_search_mode": "on",
        "prefs_timeout_seconds": 5.0,
    }
    values.update(overrides)
    return agent_run.Config(**values)


def make_runtime(
    agent: StubTransport,
    api: StubTransport,
    *,
    prefs: ScriptedPrefs | None = None,
    clock: FakeClock | None = None,
) -> agent_run.Runtime:
    fake = clock or FakeClock()
    return agent_run.Runtime(
        agent=agent,
        api=api,
        prefs=prefs or ScriptedPrefs([True]),
        sleeper=fake.sleep,
        monotonic=fake.monotonic,
        clock=FIXED_CLOCK,
    )


def run_single_prompt(
    tmp_path: Path,
    *,
    agent: StubTransport | None = None,
    api: StubTransport | None = None,
    prompt: str = "hello",
    variables: dict | None = None,
    config_overrides: dict | None = None,
    prefs: ScriptedPrefs | None = None,
) -> tuple[dict, StubTransport, StubTransport]:
    api = api or StubTransport(
        json_responses={
            agent_run.LOGIN_PATH: auth_payload(),
            agent_run.FEEDS_PATH: {},
        }
    )
    agent = agent or StubTransport(sse=sse_text(("run_id", {"run_id": "run-1"})))
    config = make_config(tmp_path, **(config_overrides or {}))
    request = {"prompt": prompt, "vars": variables or {"case_id": "case-1"}}
    transcript = agent_run.execute_case(
        request, config=config, runtime=make_runtime(agent, api, prefs=prefs)
    )
    return transcript, agent, api


def simple_turn(run_id: str, text: str) -> str:
    return sse_text(
        ("run_id", {"run_id": run_id}),
        ("delta", {"delta": text}),
        ("done", {"usage": {}}),
        ("terminator", {"reason": None}),
    )


class TestWireParsing:
    @pytest.mark.parametrize(
        "fixture", sorted(FIXTURES_DIR.glob("*.sse")), ids=lambda p: p.name
    )
    def test_recorded_fixture_parses_to_frames(self, fixture: Path) -> None:
        raw = fixture.read_text(encoding="utf-8")
        blocks = list(agent_run.iter_sse_blocks(raw.splitlines()))
        frames = [f for f in (agent_run.parse_sse_block(b) for b in blocks) if f]
        assert frames, "a fixture with no frames is not a usable capture"
        for frame in frames:
            assert frame.event in agent_run.FRAME_KINDS, frame.event
            assert isinstance(frame.data, dict)
        assert agent_run._join_blocks(blocks) == raw
        assert frames[0].event == "run_id"
        assert frames[-1].event == "terminator"

    @pytest.mark.parametrize(
        "fixture", sorted(FIXTURES_DIR.glob("*.sse")), ids=lambda p: p.name
    )
    def test_recorded_fixture_has_a_terminal_frame(self, fixture: Path) -> None:
        frames = [
            f
            for f in (
                agent_run.parse_sse_block(b)
                for b in agent_run.iter_sse_blocks(fixture.read_text().splitlines())
            )
            if f
        ]
        kinds = {f.event for f in frames}
        assert kinds & {"done", "error"}, f"{fixture.name} has no terminal frame"
        if "done" in kinds and "error" not in kinds:
            assert "delta" in kinds or "tool_call" in kinds

    def test_unknown_kind_and_comment_blocks(self) -> None:
        raw = sse_text(
            ("run_id", {"run_id": "r"}),
            ("some_future_kind", {"x": 1}),
            comment="keep-alive",
        )
        blocks = list(agent_run.iter_sse_blocks(raw.splitlines()))
        frames = [f for f in (agent_run.parse_sse_block(b) for b in blocks) if f]
        assert [f.event for f in frames] == ["run_id", "some_future_kind"]
        assert ": keep-alive" in agent_run._join_blocks(blocks)

    def test_malformed_data_is_kept_as_raw(self) -> None:
        frame = agent_run.parse_sse_block("event: delta\ndata: {not json}")
        assert frame is not None
        assert frame.event == "delta"
        assert frame.data == {"_raw": "{not json}"}

    def test_done_sentinel_is_not_a_frame(self) -> None:
        assert agent_run.parse_sse_block("data: [DONE]") is None
        assert agent_run.parse_sse_block(": keep-alive") is None
        assert agent_run.parse_sse_block("data: {\"delta\": \"hi\"}").event == "message"


class TestAnswerAssembly:
    def test_answer_is_deltas_concatenated_in_order(self) -> None:
        frames = [
            agent_run.Frame("run_id", {"run_id": "r"}),
            agent_run.Frame("delta", {"delta": "秋"}),
            agent_run.Frame("step_end", {"step_id": "s", "status": "ok"}),
            agent_run.Frame("delta", {"delta": "刀鱼"}),
            agent_run.Frame("delta", {"delta": "！"}),
            agent_run.Frame("done", {"usage": {}}),
        ]
        assert agent_run.assemble_answer(frames) == "秋刀鱼！"

    def test_answer_ignores_non_string_and_missing_deltas(self) -> None:
        frames = [
            agent_run.Frame("delta", {"delta": None}),
            agent_run.Frame("delta", {}),
            agent_run.Frame("delta", {"delta": 3}),
            agent_run.Frame("delta", {"delta": "ok"}),
        ]
        assert agent_run.assemble_answer(frames) == "ok"

    def test_error_frame_wins_over_missing_done(self) -> None:
        frames = [agent_run.Frame("error", {"error": "boom", "code": "planner_failed"})]
        assert agent_run.turn_error(frames) == {
            "code": "planner_failed",
            "message": "boom",
        }

    def test_missing_done_without_error_is_an_incomplete_stream(self) -> None:
        frames = [agent_run.Frame("delta", {"delta": "partial"})]
        assert agent_run.turn_error(frames) == {
            "code": "stream_incomplete",
            "message": "the SSE stream ended without a done or error frame",
        }

    def test_done_frame_means_no_error(self) -> None:
        assert agent_run.turn_error([agent_run.Frame("done", {"usage": {}})]) is None


class TestTranscriptShape:
    def test_transcript_matches_the_frozen_contract(self, tmp_path: Path) -> None:
        agent = StubTransport(sse=simple_turn("run-1", "你好"))
        transcript, _, _ = run_single_prompt(tmp_path, agent=agent)
        assert set(transcript) == {"status", "skip_reason", "turns", "meta"}
        assert transcript["status"] == "completed"
        assert transcript["skip_reason"] is None
        assert len(transcript["turns"]) == 1
        turn = transcript["turns"][0]
        assert set(turn) == {
            "prompt",
            "run_id",
            "session_id",
            "frames",
            "answer",
            "error",
        }
        assert turn["prompt"] == "hello"
        assert turn["run_id"] == "run-1"
        assert turn["answer"] == "你好"
        assert turn["error"] is None
        assert [f["event"] for f in turn["frames"]] == [
            "run_id",
            "delta",
            "done",
            "terminator",
        ]
        assert transcript["meta"]["eval_user_id"] == 7
        assert transcript["meta"]["eval_user_email"].startswith("eval-golden-")
        assert transcript["meta"]["started_at"] == "2026-01-01T00:00:00+00:00"

    def test_error_turn_marks_the_transcript(self, tmp_path: Path) -> None:
        agent = StubTransport(
            sse=sse_text(
                ("run_id", {"run_id": "run-1"}),
                ("error", {"error": "planner failed", "code": "planner_failed"}),
            )
        )
        transcript, _, _ = run_single_prompt(tmp_path, agent=agent)
        assert transcript["status"] == "error"
        assert transcript["turns"][0]["error"] == {
            "code": "planner_failed",
            "message": "planner failed",
        }


class TestEvalUser:
    def test_login_then_cache_reuse_across_invocations(self, tmp_path: Path) -> None:
        api = StubTransport(
            json_responses={agent_run.LOGIN_PATH: auth_payload(user_id=42)}
        )
        agent = StubTransport(sse=simple_turn("run-1", "hi"))
        config = make_config(tmp_path)
        runtime = make_runtime(agent, api)
        first = agent_run.ensure_eval_user(config, runtime)
        assert first.user_id == 42
        assert first.email == "eval-golden-20260101T000000Z@eval.nyx.local"
        assert (tmp_path / "eval_user.json").exists()

        second_agent = StubTransport(sse=simple_turn("run-2", "hi"))
        second_api = StubTransport(json_responses={})
        second = agent_run.ensure_eval_user(
            config, make_runtime(second_agent, second_api)
        )
        assert second.user_id == 42
        assert second_api.log == [], "a cached token must not re-authenticate"

    def test_unknown_user_registers_after_a_rejected_login(self, tmp_path: Path) -> None:
        class RejectingLogin(StubTransport):
            def post_json(self, path, *, headers, body, timeout):
                if path == agent_run.LOGIN_PATH:
                    self.log.append(("post", path))
                    raise agent_run.ProviderError("nope", status=401)
                return super().post_json(path, headers=headers, body=body, timeout=timeout)

        api = RejectingLogin(
            json_responses={agent_run.REGISTER_PATH: auth_payload(user_id=99)}
        )
        user = agent_run.ensure_eval_user(make_config(tmp_path), make_runtime(StubTransport(), api))
        assert user.user_id == 99
        assert [entry[1] for entry in api.log] == [
            agent_run.LOGIN_PATH,
            agent_run.REGISTER_PATH,
        ]

    def test_expired_cached_token_re_authenticates(self, tmp_path: Path) -> None:
        (tmp_path / "eval_user.json").write_text(
            json.dumps(
                {
                    "user_id": 5,
                    "email": "eval-golden-20260101T000000Z@eval.nyx.local",
                    "password": agent_run.EVAL_PASSWORD,
                    "access_token": "stale",
                    "expires_at": 1.0,
                }
            )
        )
        api = StubTransport(json_responses={agent_run.LOGIN_PATH: auth_payload(user_id=5)})
        user = agent_run.ensure_eval_user(make_config(tmp_path), make_runtime(StubTransport(), api))
        assert user.access_token == "test-access-token"
        assert [entry[1] for entry in api.log] == [agent_run.LOGIN_PATH]

    def test_auth_headers_carry_the_go_verified_user_id(self) -> None:
        user = agent_run.EvalUser(11, "e@x", "p", "tok")
        assert agent_run.auth_headers(user) == {
            "Authorization": "Bearer tok",
            "X-Nyx-User-Id": "11",
        }


class TestFixtureSeeding:
    def test_feeds_are_seeded_before_the_prompt(self, tmp_path: Path) -> None:
        shared: list[tuple] = []
        agent = StubTransport(sse=simple_turn("run-1", "ok"), log=shared)
        api = StubTransport(
            json_responses={agent_run.LOGIN_PATH: auth_payload()}, log=shared
        )
        transcript = agent_run.execute_case(
            {
                "prompt": "总结我的收藏",
                "vars": {"case_id": "rag", "fixtures": ["秋刀鱼笔记"]},
            },
            config=make_config(tmp_path),
            runtime=make_runtime(agent, api),
        )
        assert transcript["status"] == "completed"
        posts = [entry for entry in api.log if entry[0] == "post"]
        assert [entry[1] for entry in posts] == [agent_run.LOGIN_PATH, agent_run.FEEDS_PATH]
        assert posts[-1][2] == {"title": "eval-秋刀鱼笔记", "description": ""}
        feed_index = shared.index(posts[-1])
        stream_index = next(i for i, e in enumerate(shared) if e[0] == "stream")
        assert feed_index < stream_index, "fixtures must exist before the prompt"

    def test_mapping_fixtures_carry_their_description(self, tmp_path: Path) -> None:
        agent = StubTransport(sse=simple_turn("run-1", "ok"))
        _, _, api = run_single_prompt(
            tmp_path,
            agent=agent,
            variables={
                "case_id": "rag",
                "fixtures": [
                    {"title": "eval-咖啡", "description": "手冲参数：92 度水温。"}
                ],
            },
        )
        feed_posts = [e for e in api.log if e[0] == "post" and e[1] == agent_run.FEEDS_PATH]
        assert feed_posts[0][2] == {
            "title": "eval-咖啡",
            "description": "手冲参数：92 度水温。",
        }

    def test_seeding_is_idempotent(self, tmp_path: Path) -> None:
        agent = StubTransport(sse=simple_turn("run-1", "ok"))
        api = StubTransport(
            json_responses={agent_run.LOGIN_PATH: auth_payload()},
            existing_feeds=["eval-秋刀鱼笔记"],
        )
        agent_run.execute_case(
            {
                "prompt": "hello",
                "vars": {"case_id": "rag", "fixtures": ["秋刀鱼笔记"]},
            },
            config=make_config(tmp_path),
            runtime=make_runtime(agent, api),
        )
        feed_posts = [e for e in api.log if e[0] == "post" and e[1] == agent_run.FEEDS_PATH]
        assert feed_posts == []

    def test_existing_eval_prefix_is_not_doubled(self) -> None:
        assert agent_run.normalize_feed_title("eval-x") == "eval-x"
        assert agent_run.normalize_feed_title(" x ") == "eval-x"

    def test_non_list_fixtures_are_rejected(self) -> None:
        with pytest.raises(agent_run.ProviderError):
            agent_run.fixture_entries({"title": "oops"})


class TestFeedsSnapshots:
    TOOL_EFFECT = {"tool": "create_feed", "effect": "created", "description_any_of": ["拉面"]}

    def variables(self, **extra) -> dict:
        return {
            "case_id": "write-case",
            "expect": {"tool_effect": self.TOOL_EFFECT},
            **extra,
        }

    def test_snapshots_are_recorded_only_for_write_cases(self, tmp_path: Path) -> None:
        api = StubTransport(
            json_responses={agent_run.LOGIN_PATH: auth_payload()},
            feeds_pages=[
                {"data": [{"id": 1, "title": "eval-a"}], "has_more": False},
                {"data": [{"id": 1, "title": "eval-a"}, {"id": 2, "title": "eval-b"}], "has_more": False},
            ],
        )
        transcript, _, _ = run_single_prompt(
            tmp_path,
            agent=StubTransport(sse=simple_turn("run-1", "hi")),
            api=api,
            variables=self.variables(),
        )
        assert transcript["status"] == "completed"
        assert transcript["meta"]["feeds_before"] == [{"id": 1, "title": "eval-a"}]
        assert transcript["meta"]["feeds_after"] == [
            {"id": 1, "title": "eval-a"},
            {"id": 2, "title": "eval-b"},
        ]
        assert "feeds_snapshot_error" not in transcript["meta"]
        assert api.feeds_pages == []

    def test_no_snapshot_without_a_tool_effect(self, tmp_path: Path) -> None:
        api = StubTransport(
            json_responses={agent_run.LOGIN_PATH: auth_payload()},
            existing_feeds=["eval-a"],
        )
        transcript, _, _ = run_single_prompt(tmp_path, api=api)
        assert "feeds_before" not in transcript["meta"]
        assert "feeds_after" not in transcript["meta"]
        assert "feeds_snapshot_error" not in transcript["meta"]
        assert not [
            entry
            for entry in api.log
            if entry[0] == "get" and entry[1].startswith(agent_run.FEEDS_PATH)
        ]

    def test_snapshot_follows_has_more_pagination(self, tmp_path: Path) -> None:
        api = StubTransport(
            json_responses={agent_run.LOGIN_PATH: auth_payload()},
            feeds_pages=[
                {"data": [{"id": 1}], "has_more": True},
                {"data": [{"id": 2}], "has_more": False},
                {"data": [{"id": 1}, {"id": 2}, {"id": 3}], "has_more": False},
            ],
        )
        transcript, _, _ = run_single_prompt(
            tmp_path, api=api, variables=self.variables()
        )
        assert transcript["meta"]["feeds_before"] == [{"id": 1}, {"id": 2}]
        assert transcript["meta"]["feeds_after"] == [{"id": 1}, {"id": 2}, {"id": 3}]
        feeds_gets = [
            entry[1]
            for entry in api.log
            if entry[0] == "get" and entry[1].startswith(agent_run.FEEDS_PATH)
        ]
        assert feeds_gets == [
            f"{agent_run.FEEDS_PATH}?page=1&page_size={agent_run.FEEDS_PAGE_SIZE}",
            f"{agent_run.FEEDS_PATH}?page=2&page_size={agent_run.FEEDS_PAGE_SIZE}",
            f"{agent_run.FEEDS_PATH}?page=1&page_size={agent_run.FEEDS_PAGE_SIZE}",
        ]

    def test_snapshot_is_taken_after_fixtures_are_seeded(self, tmp_path: Path) -> None:
        api = StubTransport(
            json_responses={agent_run.LOGIN_PATH: auth_payload()},
            feeds_pages=[
                {"data": [], "has_more": False},
                {"data": [{"id": 1, "title": "eval-x"}], "has_more": False},
                {"data": [{"id": 1, "title": "eval-x"}], "has_more": False},
            ],
        )
        run_single_prompt(
            tmp_path,
            api=api,
            variables=self.variables(fixtures=["x"]),
        )
        log = api.log
        post_at = next(
            index
            for index, entry in enumerate(log)
            if entry[0] == "post" and entry[1] == agent_run.FEEDS_PATH
        )
        feed_gets = [
            index
            for index, entry in enumerate(log)
            if entry[0] == "get" and entry[1].startswith(agent_run.FEEDS_PATH)
        ]
        assert len(feed_gets) == 3
        assert feed_gets[0] < post_at < feed_gets[1] < feed_gets[2]

    def test_snapshot_error_is_recorded_and_does_not_abort(self, tmp_path: Path) -> None:
        class FailingFeeds(StubTransport):
            def get_json(self, path, *, headers, timeout):
                if path.startswith(agent_run.FEEDS_PATH):
                    self.log.append(("get", path))
                    raise agent_run.ProviderError("GET /api/feeds failed: ConnectError")
                return super().get_json(
                    path, headers=headers, timeout=timeout
                )

        agent = StubTransport(sse=simple_turn("run-1", "hi"))
        api = FailingFeeds(json_responses={agent_run.LOGIN_PATH: auth_payload()})
        transcript, _, _ = run_single_prompt(
            tmp_path, agent=agent, api=api, variables=self.variables()
        )
        assert transcript["status"] == "completed"
        assert transcript["turns"][0]["answer"] == "hi"
        assert "feeds_before" not in transcript["meta"]
        assert "feeds_after" not in transcript["meta"]
        error = transcript["meta"]["feeds_snapshot_error"]
        assert error.startswith("before: page 1:")
        assert "after: page 1:" in error

    def test_malformed_snapshot_page_records_the_error(self, tmp_path: Path) -> None:
        api = StubTransport(
            json_responses={agent_run.LOGIN_PATH: auth_payload()},
            feeds_pages=[
                {"has_more": False},
                {"data": [{"id": 1}], "has_more": False},
            ],
        )
        transcript, _, _ = run_single_prompt(
            tmp_path, api=api, variables=self.variables()
        )
        assert "feeds_before" not in transcript["meta"]
        assert transcript["meta"]["feeds_after"] == [{"id": 1}]
        assert "no data list" in transcript["meta"]["feeds_snapshot_error"]


class TestDialogueRunIds:
    def test_single_turn_omits_run_id(self, tmp_path: Path) -> None:
        agent = StubTransport(sse=simple_turn("minted-1", "hi"))
        _, _, _ = run_single_prompt(tmp_path, agent=agent)
        stream = next(e for e in agent.log if e[0] == "stream")
        assert "run_id" not in stream[2]

    def test_new_session_turns_omit_run_id_and_continuations_echo_it(
        self, tmp_path: Path
    ) -> None:
        class TwoTurnAgent(StubTransport):
            def __init__(self) -> None:
                super().__init__()
                self.index = 0

            def stream_sse(self, path, *, headers, body, timeout, on_frame):
                self.index += 1
                self.sse = (
                    simple_turn("minted-1", "seed")
                    if self.index == 1
                    else simple_turn("minted-1", "recall")
                )
                return super().stream_sse(
                    path, headers=headers, body=body, timeout=timeout, on_frame=on_frame
                )

        agent = TwoTurnAgent()
        transcript, _, _ = run_single_prompt(
            tmp_path,
            agent=agent,
            variables={
                "case_id": "dialogue",
                "dialogue": [
                    {"prompt": "我最近爱吃秋刀鱼。", "new_session": True},
                    {"prompt": "我喜欢吃什么？", "new_session": False},
                ],
            },
        )
        streams = [e for e in agent.log if e[0] == "stream"]
        assert len(streams) == 2
        assert "run_id" not in streams[0][2]
        assert streams[1][2]["run_id"] == "minted-1"
        assert [t["prompt"] for t in transcript["turns"]] == [
            "我最近爱吃秋刀鱼。",
            "我喜欢吃什么？",
        ]
        assert [t["run_id"] for t in transcript["turns"]] == ["minted-1", "minted-1"]
        assert [t["session_id"] for t in transcript["turns"]] == ["minted-1", "minted-1"]

    def test_three_turn_memory_script_opens_three_sessions(self, tmp_path: Path) -> None:
        class ThreeTurnAgent(StubTransport):
            def __init__(self) -> None:
                super().__init__()
                self.index = 0

            def stream_sse(self, path, *, headers, body, timeout, on_frame):
                self.index += 1
                self.sse = sse_text(
                    ("run_id", {"run_id": f"session-{self.index}"}),
                    ("delta", {"delta": "ok"}),
                    ("done", {"usage": {}}),
                )
                return super().stream_sse(
                    path, headers=headers, body=body, timeout=timeout, on_frame=on_frame
                )

        agent = ThreeTurnAgent()
        transcript, _, _ = run_single_prompt(
            tmp_path,
            agent=agent,
            variables={
                "case_id": "memory",
                "poll_prefs": True,
                "dialogue": [
                    {"prompt": "我最近爱吃秋刀鱼。", "new_session": True},
                    {"prompt": "我喜欢吃什么？", "new_session": True},
                    {"prompt": "推荐吃的给我。", "new_session": True},
                ],
            },
        )
        streams = [e for e in agent.log if e[0] == "stream"]
        assert all("run_id" not in entry[2] for entry in streams)
        assert [t["run_id"] for t in transcript["turns"]] == [
            "session-1",
            "session-2",
            "session-3",
        ]

    def test_first_turn_defaults_to_a_new_session(self, tmp_path: Path) -> None:
        agent = StubTransport(sse=simple_turn("minted-1", "hi"))
        _, _, _ = run_single_prompt(
            tmp_path,
            agent=agent,
            variables={
                "case_id": "d",
                "dialogue": [{"prompt": "first"}, {"prompt": "second", "new_session": False}],
            },
        )
        streams = [e for e in agent.log if e[0] == "stream"]
        assert "run_id" not in streams[0][2]
        assert streams[1][2]["run_id"] == "minted-1"

    def test_dialogue_entry_without_prompt_is_rejected(self, tmp_path: Path) -> None:
        agent = StubTransport(sse=simple_turn("r", "hi"))
        with pytest.raises(agent_run.ProviderError):
            run_single_prompt(
                tmp_path,
                agent=agent,
                variables={"case_id": "d", "dialogue": [{"new_session": True}]},
            )


class TestApprovalHandling:
    APPROVAL_SSE = sse_text(
        ("run_id", {"run_id": "run-9"}),
        (
            "confirm_request",
            {
                "run_id": "run-9",
                "step_id": "s1",
                "tool": "create_feed",
                "args": {"title": "eval-x"},
                "approval_id": "ap-1",
                "expires_at": 4102444800.0,
            },
        ),
        ("tool_call", {"step_id": "s1", "call_id": "c1", "name": "create_feed", "args": {}}),
        ("tool_result", {"step_id": "s1", "call_id": "c1", "ok": True, "result": {}}),
        ("delta", {"delta": "done"}),
        ("done", {"usage": {}}),
    )

    def test_confirm_request_is_approved_immediately(self, tmp_path: Path) -> None:
        agent = StubTransport(sse=self.APPROVAL_SSE)
        transcript, _, _ = run_single_prompt(tmp_path, agent=agent)
        approvals = [e for e in agent.log if e[0] == "post"]
        assert len(approvals) == 1
        path, body, headers = approvals[0][1], approvals[0][2], approvals[0][3]
        assert path == agent_run.APPROVALS_PATH
        assert body == {
            "run_id": "run-9",
            "approval_id": "ap-1",
            "decision": "approve",
        }
        assert headers["X-Nyx-User-Id"] == "7"
        assert headers["Authorization"] == "Bearer test-access-token"
        assert transcript["status"] == "completed"

    def test_approval_lands_while_the_run_is_paused(self, tmp_path: Path) -> None:
        agent = StubTransport(sse=self.APPROVAL_SSE)
        run_single_prompt(tmp_path, agent=agent)
        frames_and_posts = [
            (entry[0], entry[1]) for entry in agent.log if entry[0] in ("frame", "post")
        ]
        assert frames_and_posts == [
            ("frame", "run_id"),
            ("frame", "confirm_request"),
            ("post", agent_run.APPROVALS_PATH),
            ("frame", "tool_call"),
            ("frame", "tool_result"),
            ("frame", "delta"),
            ("frame", "done"),
        ]

    def test_deny_is_never_sent(self, tmp_path: Path) -> None:
        agent = StubTransport(sse=self.APPROVAL_SSE)
        run_single_prompt(tmp_path, agent=agent)
        decisions = {e[2]["decision"] for e in agent.log if e[0] == "post"}
        assert decisions == {"approve"}

    def test_approval_failure_is_loud(self, tmp_path: Path) -> None:
        class RejectingApproval(StubTransport):
            def post_json(self, path, *, headers, body, timeout):
                if path == agent_run.APPROVALS_PATH:
                    raise agent_run.ProviderError("approval store down", status=503)
                return super().post_json(path, headers=headers, body=body, timeout=timeout)

        agent = RejectingApproval(sse=self.APPROVAL_SSE)
        with pytest.raises(agent_run.ProviderError) as excinfo:
            run_single_prompt(tmp_path, agent=agent)
        assert "approval decision failed" in str(excinfo.value)

    def test_incomplete_confirm_frame_is_rejected(self, tmp_path: Path) -> None:
        agent = StubTransport(
            sse=sse_text(("run_id", {"run_id": "r"}), ("confirm_request", {"step_id": "s"}))
        )
        with pytest.raises(agent_run.ProviderError):
            run_single_prompt(tmp_path, agent=agent)


class TestPrefsPolling:
    def dialogue_vars(self) -> dict:
        return {
            "case_id": "memory",
            "poll_prefs": True,
            "dialogue": [
                {"prompt": "我最近爱吃秋刀鱼。", "new_session": True},
                {"prompt": "我喜欢吃什么？", "new_session": True},
            ],
        }

    class CountingAgent(StubTransport):
        def __init__(self, sse: str) -> None:
            super().__init__(sse=sse)
            self.index = 0

        def stream_sse(self, path, *, headers, body, timeout, on_frame):
            self.index += 1
            self.sse = sse_text(
                ("run_id", {"run_id": f"s-{self.index}"}),
                ("delta", {"delta": "ok"}),
                ("done", {"usage": {}}),
            )
            return super().stream_sse(
                path, headers=headers, body=body, timeout=timeout, on_frame=on_frame
            )

    def test_landed_immediately(self, tmp_path: Path) -> None:
        agent = self.CountingAgent("")
        prefs = ScriptedPrefs([True])
        transcript, _, _ = run_single_prompt(
            tmp_path, agent=agent, variables=self.dialogue_vars(), prefs=prefs
        )
        assert transcript["status"] == "completed"
        assert prefs.calls == [(7, None)]
        assert len(transcript["turns"]) == 2

    def test_landed_after_delay(self, tmp_path: Path) -> None:
        agent = self.CountingAgent("")
        clock = FakeClock()
        prefs = ScriptedPrefs([False, False, True])
        api = StubTransport(json_responses={agent_run.LOGIN_PATH: auth_payload()})
        transcript = agent_run.execute_case(
            {"prompt": "", "vars": self.dialogue_vars()},
            config=make_config(tmp_path),
            runtime=agent_run.Runtime(
                agent=agent,
                api=api,
                prefs=prefs,
                sleeper=clock.sleep,
                monotonic=clock.monotonic,
                clock=FIXED_CLOCK,
            ),
        )
        assert transcript["status"] == "completed"
        assert len(prefs.calls) == 3
        assert clock.now >= 2.0, "two poll intervals must have elapsed"
        assert len(transcript["turns"]) == 2

    def test_timeout_skips_and_stops_the_dialogue(self, tmp_path: Path) -> None:
        agent = self.CountingAgent("")
        clock = FakeClock()
        prefs = ScriptedPrefs([])
        api = StubTransport(json_responses={agent_run.LOGIN_PATH: auth_payload()})
        transcript = agent_run.execute_case(
            {"prompt": "", "vars": self.dialogue_vars()},
            config=make_config(tmp_path, prefs_timeout_seconds=3.0),
            runtime=agent_run.Runtime(
                agent=agent,
                api=api,
                prefs=prefs,
                sleeper=clock.sleep,
                monotonic=clock.monotonic,
                clock=FIXED_CLOCK,
            ),
        )
        assert transcript["status"] == "skipped"
        assert transcript["skip_reason"] == "preference extraction timeout"
        assert len(transcript["turns"]) == 1, "recall turns must not run after a skip"
        assert agent.index == 1
        assert clock.now >= 3.0

    def test_key_filter_is_forwarded_to_the_reader(self, tmp_path: Path) -> None:
        agent = self.CountingAgent("")
        prefs = ScriptedPrefs([True])
        run_single_prompt(
            tmp_path,
            agent=agent,
            variables={
                "case_id": "memory",
                "poll_prefs": {"key": "snack"},
                "dialogue": [{"prompt": "seed", "new_session": True}],
            },
            prefs=prefs,
        )
        assert prefs.calls == [(7, "snack")]

    def test_no_poll_without_the_var(self, tmp_path: Path) -> None:
        agent = self.CountingAgent("")
        prefs = ScriptedPrefs([True])
        transcript, _, _ = run_single_prompt(
            tmp_path,
            agent=agent,
            variables={
                "case_id": "memory",
                "dialogue": [{"prompt": "seed", "new_session": True}],
            },
            prefs=prefs,
        )
        assert prefs.calls == []
        assert transcript["status"] == "completed"

    def test_wait_for_pref_row_returns_false_on_timeout(self) -> None:
        clock = FakeClock()
        landed = agent_run.wait_for_pref_row(
            ScriptedPrefs([]),
            1,
            None,
            timeout=2.0,
            interval=1.0,
            sleeper=clock.sleep,
            monotonic=clock.monotonic,
        )
        assert landed is False

    def test_wait_for_pref_row_returns_true_before_the_deadline(self) -> None:
        clock = FakeClock()
        landed = agent_run.wait_for_pref_row(
            ScriptedPrefs([False, True]),
            1,
            None,
            timeout=10.0,
            interval=1.0,
            sleeper=clock.sleep,
            monotonic=clock.monotonic,
        )
        assert landed is True
        assert clock.now == pytest.approx(1.0)


class TestPreflight:
    def test_unreachable_go_api_names_the_url(self, tmp_path: Path) -> None:
        runtime = make_runtime(StubTransport(), UnreachableTransport())
        with pytest.raises(agent_run.ProviderError) as excinfo:
            agent_run.preflight(make_config(tmp_path), runtime)
        message = str(excinfo.value)
        assert "Go API unreachable" in message
        assert "http://go.test/api/health" in message

    def test_unreachable_agent_names_the_url(self, tmp_path: Path) -> None:
        runtime = make_runtime(UnreachableTransport(), StubTransport())
        with pytest.raises(agent_run.ProviderError) as excinfo:
            agent_run.preflight(make_config(tmp_path), runtime)
        message = str(excinfo.value)
        assert "agent unreachable" in message
        assert "http://agent.test/healthz" in message

    def test_unhealthy_status_names_the_url_and_code(self, tmp_path: Path) -> None:
        runtime = make_runtime(StubTransport(status_code=503), StubTransport())
        with pytest.raises(agent_run.ProviderError) as excinfo:
            agent_run.preflight(make_config(tmp_path), runtime)
        message = str(excinfo.value)
        assert "http://agent.test/healthz" in message
        assert "503" in message


PROMPTFOO_CONFIG_JSON = json.dumps(
    {
        "id": "exec:python /repo/evals/providers/agent_run.py",
        "config": {"basePath": "/repo"},
        "env": {},
    }
)
PROMPTFOO_CONTEXT_JSON = json.dumps(
    {
        "vars": {
            "case_id": "case-1",
            "fixtures": ["秋刀鱼"],
            "__evalId": "eval-abc",
            "__evalStepId": "test-0-prompt-0-repeat-0",
            "__repeatIndex": 0,
        },
        "prompt": {"raw": "{{prompt}}", "label": "{{prompt}}", "config": {}},
        "test": {"vars": {"case_id": "case-1"}, "assert": []},
        "repeatIndex": 0,
        "testIdx": 0,
        "evaluationId": "eval-abc",
    }
)


class TestInvocation:
    def test_promptfoo_argv_context_is_the_request(self) -> None:
        request = agent_run.parse_invocation(
            ["hello probe", PROMPTFOO_CONFIG_JSON, PROMPTFOO_CONTEXT_JSON], ""
        )
        assert request["prompt"] == "hello probe"
        assert request["vars"]["case_id"] == "case-1"
        assert request["vars"]["fixtures"] == ["秋刀鱼"]
        assert request["context"]["repeatIndex"] == 0

    def test_argv_prompt_without_context(self) -> None:
        request = agent_run.parse_invocation(["hello probe"], "")
        assert request == {"prompt": "hello probe", "vars": {}}

    def test_stdin_json_still_works_for_manual_runs(self) -> None:
        request = agent_run.parse_invocation(
            [], json.dumps({"prompt": "hi", "vars": {"case_id": "c"}})
        )
        assert request["prompt"] == "hi"
        assert request["vars"]["case_id"] == "c"

    def test_no_input_at_all_is_loud(self) -> None:
        with pytest.raises(agent_run.ProviderError):
            agent_run.parse_invocation([], "")

    def test_malformed_stdin_is_loud(self) -> None:
        with pytest.raises(agent_run.ProviderError):
            agent_run.parse_invocation([], "not json")


class TestExecProtocol:
    def test_stdout_is_the_bare_transcript(self, tmp_path: Path, monkeypatch, capsys) -> None:
        agent = StubTransport(sse=simple_turn("run-1", "hello"))
        api = StubTransport(json_responses={agent_run.LOGIN_PATH: auth_payload()})
        monkeypatch.setattr(agent_run, "build_runtime", lambda config: make_runtime(agent, api))
        monkeypatch.setenv("EVAL_ARTIFACTS_DIR", str(tmp_path))
        monkeypatch.setenv("EVAL_RUN_STAMP", "20260101T000000Z")
        monkeypatch.setattr(
            sys,
            "argv",
            ["agent_run.py", "hello", PROMPTFOO_CONFIG_JSON, PROMPTFOO_CONTEXT_JSON],
        )

        class Stdin:
            def read(self) -> str:
                return ""

        monkeypatch.setattr(sys, "stdin", Stdin())
        assert agent_run.main() == 0
        captured = capsys.readouterr()
        assert captured.err == ""
        transcript = json.loads(captured.out)
        assert set(transcript) == {"status", "skip_reason", "turns", "meta"}
        assert transcript["status"] == "completed"
        assert transcript["turns"][0]["answer"] == "hello"

    def test_preflight_failure_exits_non_zero_with_the_url(
        self, tmp_path: Path, monkeypatch, capsys
    ) -> None:
        monkeypatch.setattr(
            agent_run,
            "build_runtime",
            lambda config: make_runtime(UnreachableTransport(), StubTransport()),
        )
        monkeypatch.setenv("EVAL_ARTIFACTS_DIR", str(tmp_path))
        monkeypatch.setenv("EVAL_GO_API_URL", "http://go.test")
        monkeypatch.setenv("EVAL_AGENT_URL", "http://agent.test")
        monkeypatch.setattr(
            sys,
            "argv",
            ["agent_run.py", "hello", PROMPTFOO_CONFIG_JSON, PROMPTFOO_CONTEXT_JSON],
        )

        class Stdin:
            def read(self) -> str:
                return ""

        monkeypatch.setattr(sys, "stdin", Stdin())
        assert agent_run.main() == 1
        captured = capsys.readouterr()
        assert captured.out == ""
        assert "http://agent.test/healthz" in captured.err

    def test_request_without_prompt_or_dialogue_is_a_loud_exit(
        self, tmp_path: Path, monkeypatch, capsys
    ) -> None:
        agent = StubTransport(sse=simple_turn("run-1", "hello"))
        api = StubTransport(json_responses={agent_run.LOGIN_PATH: auth_payload()})
        monkeypatch.setattr(agent_run, "build_runtime", lambda config: make_runtime(agent, api))
        monkeypatch.setenv("EVAL_ARTIFACTS_DIR", str(tmp_path))
        monkeypatch.setattr(sys, "argv", ["agent_run.py"])
        monkeypatch.setattr(sys, "stdin", type("S", (), {"read": lambda self: ""})())
        assert agent_run.main() == 2
        assert "no prompt received" in capsys.readouterr().err


class TestArtifacts:
    def test_raw_capture_and_transcript_are_written(self, tmp_path: Path) -> None:
        agent = StubTransport(sse=simple_turn("run-1", "hi"))
        run_single_prompt(tmp_path, agent=agent)
        raw = (tmp_path / "samples" / "case-1-1.sse").read_text()
        assert raw == agent.sse
        transcript = json.loads((tmp_path / "samples" / "case-1-1.json").read_text())
        assert transcript["turns"][0]["answer"] == "hi"

    def test_invocation_counter_advances(self, tmp_path: Path) -> None:
        for _ in range(2):
            run_single_prompt(tmp_path, agent=StubTransport(sse=simple_turn("r", "hi")))
        assert agent_run.next_sample_index(tmp_path, "case-1") == 3
        assert (tmp_path / "samples" / "case-1-2.sse").exists()

    def test_missing_samples_dir_starts_at_one(self) -> None:
        assert agent_run.next_sample_index(Path("/nonexistent"), "x") == 1

    def test_turn_markers_separate_dialogue_bytes(self, tmp_path: Path) -> None:
        class TwoTurnAgent(StubTransport):
            def __init__(self) -> None:
                super().__init__()
                self.index = 0

            def stream_sse(self, path, *, headers, body, timeout, on_frame):
                self.index += 1
                self.sse = simple_turn(f"run-{self.index}", f"turn{self.index}")
                return super().stream_sse(
                    path, headers=headers, body=body, timeout=timeout, on_frame=on_frame
                )

        agent = TwoTurnAgent()
        run_single_prompt(
            tmp_path,
            agent=agent,
            variables={
                "case_id": "dialogue",
                "dialogue": [
                    {"prompt": "one", "new_session": True},
                    {"prompt": "two", "new_session": False},
                ],
            },
        )
        raw = (tmp_path / "samples" / "dialogue-1.sse").read_text()
        assert ": --- turn 2 ---" in raw
        parsed = [
            f
            for f in (
                agent_run.parse_sse_block(b)
                for b in agent_run.iter_sse_blocks(raw.splitlines())
            )
            if f
        ]
        assert [f.event for f in parsed].count("run_id") == 2
        assert "turn1" in agent_run.assemble_answer(parsed)


class TestConfig:
    def test_frozen_env_names_are_honoured(self, tmp_path: Path) -> None:
        config = agent_run.resolve_config(
            {
                "EVAL_GO_API_URL": "http://go:1/",
                "EVAL_AGENT_URL": "http://agent:2/",
                "EVAL_ARTIFACTS_DIR": str(tmp_path),
                "EVAL_RUN_STAMP": "20260101T010203Z",
                "EVAL_DB_DSN": "postgresql://other",
                "EVAL_WEB_SEARCH": "off",
                "EVAL_PREFS_TIMEOUT_SECONDS": "7",
            }
        )
        assert config.go_api_url == "http://go:1"
        assert config.agent_url == "http://agent:2"
        assert config.artifacts_dir == tmp_path
        assert config.run_stamp == "20260101T010203Z"
        assert config.db_dsn == "postgresql://other"
        assert config.web_search_mode == "off"
        assert config.prefs_timeout_seconds == 7.0

    def test_defaults_follow_the_dev_stack(self) -> None:
        config = agent_run.resolve_config({})
        assert config.go_api_url == "http://localhost:8180"
        assert config.agent_url == "http://localhost:9100"
        assert config.db_dsn == agent_run.DEFAULT_DB_DSN
        assert config.prefs_timeout_seconds == 60.0
        assert config.web_search_mode == "on"

    def test_stamp_is_persisted_for_the_next_process(self, tmp_path: Path) -> None:
        first = agent_run.resolve_config({"EVAL_ARTIFACTS_DIR": str(tmp_path)})
        second = agent_run.resolve_config({"EVAL_ARTIFACTS_DIR": str(tmp_path)})
        assert first.run_stamp == second.run_stamp
        assert (tmp_path / "run_stamp.txt").read_text().strip() == first.run_stamp

    def test_env_stamp_wins_over_the_persisted_one(self, tmp_path: Path) -> None:
        (tmp_path / "run_stamp.txt").write_text("19990101T000000Z\n")
        config = agent_run.resolve_config(
            {"EVAL_ARTIFACTS_DIR": str(tmp_path), "EVAL_RUN_STAMP": "20260101T000000Z"}
        )
        assert config.run_stamp == "20260101T000000Z"
        assert agent_run.eval_user_email(config) == (
            "eval-golden-20260101T000000Z@eval.nyx.local"
        )


class TestRecordedFixtureShapes:
    def frames_of(self, name: str) -> list[agent_run.Frame]:
        raw = (FIXTURES_DIR / name).read_text(encoding="utf-8")
        return [
            f
            for f in (
                agent_run.parse_sse_block(b)
                for b in agent_run.iter_sse_blocks(raw.splitlines())
            )
            if f
        ]

    def test_rag_only_shape(self) -> None:
        frames = self.frames_of("sse_rag_only.sse")
        kinds = [f.event for f in frames]
        assert "plan" in kinds
        assert "error" not in kinds

    def test_rag_create_feed_shape_carries_the_approval_continuation(self) -> None:
        frames = self.frames_of("sse_rag_create_feed.sse")
        kinds = [f.event for f in frames]
        assert "confirm_request" in kinds
        confirm_at = kinds.index("confirm_request")
        confirm = frames[confirm_at]
        assert confirm.data["tool"] == "create_feed"
        assert confirm.data["run_id"]
        assert confirm.data["approval_id"]
        assert confirm.data["expires_at"] > 0
        continuation = kinds[confirm_at:]
        assert "tool_call" in continuation
        assert "tool_result" in continuation, "the approved call must show its result"
        tool_result = next(f for f in frames if f.event == "tool_result")
        assert tool_result.data["ok"] is True
        assert "done" in kinds

    def test_refusal_shape_has_no_tool_calls(self) -> None:
        frames = self.frames_of("sse_refusal_direct_answer.sse")
        assert not [f for f in frames if f.event == "tool_call"]

    def test_direct_answer_shape(self) -> None:
        frames = self.frames_of("sse_direct_answer.sse")
        assert not [f for f in frames if f.event == "tool_call"]
        assert agent_run.assemble_answer(frames)
        assert agent_run.turn_error(frames) is None

    @pytest.mark.parametrize(
        "fixture", sorted(FIXTURES_DIR.glob("sse_*_web-*.sse")), ids=lambda p: p.name
    )
    def test_web_shape_mode_in_the_filename_matches_the_frames(self, fixture: Path) -> None:
        mode = "on" if "web-on" in fixture.name else "off"
        frames = self.frames_of(fixture.name)
        called = {
            f.data.get("name") for f in frames if f.event == "tool_call"
        }
        if mode == "on":
            assert "web_search" in called, fixture.name
        else:
            assert "web_search" not in called, fixture.name

    def test_memory_dialogue_has_three_sessions(self) -> None:
        frames = self.frames_of("sse_memory_dialogue_3turn.sse")
        run_ids = [f.data["run_id"] for f in frames if f.event == "run_id"]
        assert len(run_ids) == 3
        assert len(set(run_ids)) == 3, "each new-session turn mints its own id"
        raw = (FIXTURES_DIR / "sse_memory_dialogue_3turn.sse").read_text()
        assert raw.count(": --- turn ") == 2

    def test_memory_dialogue_with_landed_prefs_recalls_in_new_sessions(self) -> None:
        frames = self.frames_of("sse_memory_dialogue_prefs_landed.sse")
        run_ids = [f.data["run_id"] for f in frames if f.event == "run_id"]
        assert len(set(run_ids)) == 3
        answer = agent_run.assemble_answer(frames)
        assert answer.count("秋刀鱼") >= 2

    def test_continuation_shape_echoes_one_session_id(self) -> None:
        frames = self.frames_of("sse_dialogue_continuation.sse")
        run_ids = [f.data["run_id"] for f in frames if f.event == "run_id"]
        assert len(run_ids) == 2
        assert len(set(run_ids)) == 1, "a continuation stays in the same session"

    def test_combined_shape_contains_multiple_tool_kinds(self) -> None:
        raw = (FIXTURES_DIR / "sse_combined_multi_step_web-on.sse")
        if not raw.exists():
            raw = FIXTURES_DIR / "sse_combined_multi_step_web-off.sse"
        frames = [
            f
            for f in (
                agent_run.parse_sse_block(b)
                for b in agent_run.iter_sse_blocks(raw.read_text().splitlines())
            )
            if f
        ]
        assert len({f.data.get("name") for f in frames if f.event == "tool_call"}) >= 2


class TestStamp:
    def test_utc_stamp_is_compact_and_sortable(self) -> None:
        stamp = agent_run.utc_stamp(datetime(2026, 10, 16, 8, 30, 3, tzinfo=timezone.utc))
        assert stamp == "20261016T083003Z"
        assert 3 < len(stamp) <= 30, "the stamp is embedded in a 50-char username"


class TestPrerequisiteGating:
    def test_budget_case_skips_without_the_stack(self, tmp_path, monkeypatch):
        monkeypatch.delenv("EVAL_BUDGET_STACK", raising=False)
        transcript, agent, api = run_single_prompt(
            tmp_path,
            variables={"case_id": "budget-case", "requires": "budget_stack"},
        )
        assert transcript["status"] == "skipped"
        assert "budget_stack" in transcript["skip_reason"]
        assert transcript["turns"] == []
        assert agent.log == []
        assert api.log == []

    def test_budget_case_runs_when_the_stack_is_declared(self, tmp_path, monkeypatch):
        monkeypatch.setenv("EVAL_BUDGET_STACK", "1")
        transcript, agent, api = run_single_prompt(
            tmp_path,
            agent=StubTransport(sse=simple_turn("run-1", "ok")),
            variables={"case_id": "budget-case", "requires": "budget_stack"},
        )
        assert transcript["status"] == "completed"
        assert transcript["turns"]

    def test_unknown_prerequisite_skips_with_its_name(self, tmp_path):
        transcript, _, _ = run_single_prompt(
            tmp_path,
            variables={"case_id": "odd-case", "requires": "quantum_stack"},
        )
        assert transcript["status"] == "skipped"
        assert "quantum_stack" in transcript["skip_reason"]


class TestArtifactsDirResolution:
    def test_relative_artifacts_dir_is_anchored_at_the_repo_root(self):
        config = agent_run.resolve_config(
            {"EVAL_ARTIFACTS_DIR": "evals/artifacts/probe", "EVAL_RUN_STAMP": "probe"}
        )
        assert config.artifacts_dir.is_absolute()
        assert config.artifacts_dir == agent_run.REPO_ROOT / "evals" / "artifacts" / "probe"

    def test_absolute_artifacts_dir_is_kept(self, tmp_path):
        config = agent_run.resolve_config(
            {"EVAL_ARTIFACTS_DIR": str(tmp_path), "EVAL_RUN_STAMP": "probe"}
        )
        assert config.artifacts_dir == tmp_path
