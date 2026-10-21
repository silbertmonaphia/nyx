"""Corpus contract tests: the golden-set case files and their promptfoo loader.

Model-free; run with the agent interpreter:

    /home/smona/nyx/backend/agent_py/.venv/bin/python -m pytest evals/tests -q

Tokens are validated against the production catalog: the MCP registry
(`backend/internal/mcp/model.go`) plus the agent-native web tools
(`backend/agent_py/agent/web_tools.py`).
"""

import importlib.util
import pathlib
import re

import pytest
import yaml

EVALS_DIR = pathlib.Path(__file__).resolve().parents[1]
CORPUS_PATHS = {
    "main": EVALS_DIR / "cases" / "main.yaml",
    "security": EVALS_DIR / "cases" / "security.yaml",
}

STEP_KINDS = {"rag", "llm", "react"}
TOOL_NAMES = {
    "list_feeds",
    "get_feed",
    "create_feed",
    "update_feed",
    "delete_feed",
    "web_search",
    "web_fetch",
}
KNOWN_TOKENS = STEP_KINDS | TOOL_NAMES

KNOWN_REQUIRES = {None, "budget_stack"}
GRADER_REFS = {
    "file://graders/plan_shape.py",
    "file://graders/answer.py",
    "file://graders/security.py",
    "file://graders/tool_effect.py",
    "file://graders/terminal.py",
}
GRADER_FOR_EXPECT_KEY = {
    "plan": "file://graders/plan_shape.py",
    "answer_contains": "file://graders/answer.py",
    "answer_forbidden": "file://graders/answer.py",
    "turns": "file://graders/answer.py",
    "no_destructive_tool_call": "file://graders/security.py",
    "allowed_tools": "file://graders/security.py",
    "no_prompt_leak": "file://graders/security.py",
    "payload_marker": "file://graders/security.py",
    "require_confirm_request": "file://graders/security.py",
    "no_cross_user_tool_call": "file://graders/security.py",
    "tool_effect": "file://graders/tool_effect.py",
    "error_code": "file://graders/terminal.py",
}
EXPECT_KEYS = set(GRADER_FOR_EXPECT_KEY)

REQUIRED_MAIN_IDS = {
    "main-rag-summary-recent",
    "main-rag-create-feed",
    "main-rag-update-feed",
    "main-rag-delete-feed",
    "main-rag-draft-no-publish",
    "main-web-weather-modes",
    "main-web-force-search",
    "main-all-tools-combined",
    "main-refusal-direct-answer",
    "main-budget-exceeded",
    "main-memory-preference-recall",
}

ID_RE = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")


def _load_loader():
    spec = importlib.util.spec_from_file_location(
        "eval_corpus_loader", EVALS_DIR / "cases" / "loader.py"
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


LOADER = _load_loader()
CASES = {line: LOADER.load_corpus(path) for line, path in CORPUS_PATHS.items()}
ALL_CASES = [(line, case) for line, cases in CASES.items() for case in cases]
ALL_IDS = [case["id"] for _, case in ALL_CASES]


def _param_cases(line):
    return pytest.mark.parametrize(
        "case", CASES[line], ids=[case["id"] for case in CASES[line]]
    )


def _param_all_cases():
    return pytest.mark.parametrize(
        "case", [case for _, case in ALL_CASES], ids=ALL_IDS
    )


def _plan_lists(case):
    plan = case["expect"].get("plan")
    if plan is None:
        return []
    return [plan] if isinstance(plan, list) else list(plan.values())


def test_corpus_files_are_non_empty():
    assert CASES["main"], "main corpus must contain cases"
    assert CASES["security"], "security corpus must contain cases"


def test_case_ids_are_unique_and_stable():
    assert len(ALL_IDS) == len(set(ALL_IDS))
    for case_id in ALL_IDS:
        assert ID_RE.match(case_id), f"{case_id} is not kebab-case"


@pytest.mark.parametrize("line", sorted(CORPUS_PATHS))
def test_every_case_declares_its_line(line):
    for case in CASES[line]:
        assert case["line"] == line, case["id"]


@_param_all_cases()
def test_case_shape_and_floor(case):
    case_id = case["id"]
    assert isinstance(case.get("prompt"), str) and case["prompt"].strip(), case_id
    expect = case.get("expect")
    assert isinstance(expect, dict) and expect, f"{case_id}: expect must be a non-empty mapping"
    assert set(expect) <= EXPECT_KEYS, f"{case_id}: unexpected expect keys {sorted(set(expect) - EXPECT_KEYS)}"
    min_rate = case.get("min_rate")
    assert isinstance(min_rate, (int, float)) and not isinstance(min_rate, bool), case_id
    assert 0.0 <= float(min_rate) <= 1.0, case_id
    assert case.get("requires") in KNOWN_REQUIRES, case_id
    if float(min_rate) < 1.0:
        assert isinstance(case.get("note"), str) and case["note"].strip(), (
            f"{case_id}: a floor below 1.0 needs a note justifying it"
        )


@_param_all_cases()
def test_plan_tokens_are_catalog_names_or_step_kinds(case):
    case_id = case["id"]
    for tokens in _plan_lists(case):
        assert tokens, f"{case_id}: empty plan expectation"
        unknown = sorted(set(tokens) - KNOWN_TOKENS)
        assert not unknown, f"{case_id}: unknown plan tokens {unknown}"


@_param_all_cases()
def test_plan_expectation_ends_with_the_answer_step(case):
    case_id = case["id"]
    for tokens in _plan_lists(case):
        assert tokens[-1] == "llm", f"{case_id}: wire-order plan must end with llm, got {tokens}"


@_param_cases("main")
def test_mode_keyed_plan_lists_are_paired(case):
    plan = case["expect"].get("plan")
    if not isinstance(plan, dict):
        pytest.skip("case declares a single plan list")
    assert set(plan) == {"on", "off"}, f"{case['id']}: mode map keys {sorted(plan)}"
    for mode, tokens in plan.items():
        assert tokens, f"{case['id']}: empty {mode} expectation"


def test_weather_case_is_the_mode_tripwire():
    weather = next(case for case in CASES["main"] if case["id"] == "main-web-weather-modes")
    plan = weather["expect"]["plan"]
    assert plan["on"] == ["web_search", "llm"]
    assert plan["off"] == ["llm"]


@_param_all_cases()
def test_answer_expectations_are_well_formed(case):
    case_id = case["id"]
    expect = case["expect"]
    for key in ("answer_contains", "answer_forbidden"):
        value = expect.get(key)
        if value is None:
            continue
        assert isinstance(value, list) and value, f"{case_id}: {key} must be a non-empty list"
        assert all(isinstance(item, str) and item for item in value), case_id
    for turn in expect.get("turns", []):
        assert isinstance(turn.get("index"), int) and turn["index"] >= 1, case_id
        assert set(turn) - {"index"}, f"{case_id}: turn assertion has no content"
        assert set(turn) <= {"index", "answer_contains", "answer_forbidden"}, case_id


@_param_all_cases()
def test_structural_expectations_are_booleans_or_catalog_tools(case):
    case_id = case["id"]
    expect = case["expect"]
    for key in ("no_destructive_tool_call", "no_prompt_leak"):
        if key in expect:
            assert expect[key] is True, f"{case_id}: {key} is only meaningful as true"
    if "allowed_tools" in expect:
        allowed = expect["allowed_tools"]
        assert isinstance(allowed, list) and allowed, case_id
        assert set(allowed) <= TOOL_NAMES, f"{case_id}: unknown allowed tools"
    if "error_code" in expect:
        assert isinstance(expect["error_code"], str) and expect["error_code"], case_id


@_param_all_cases()
def test_fixtures_match_the_feeds_api_input_shape(case):
    fixtures = (case.get("vars") or {}).get("fixtures")
    if fixtures is None:
        pytest.skip("case seeds no fixtures")
    assert isinstance(fixtures, list) and fixtures, case["id"]
    for fixture in fixtures:
        assert set(fixture) == {"title", "description"}, f"{case['id']}: {sorted(fixture)}"
        title = fixture["title"]
        description = fixture["description"]
        assert title.startswith("eval-"), f"{case['id']}: fixture title must be eval-prefixed"
        assert len(title) <= 100, f"{case['id']}: fixture title over the feeds API cap"
        assert isinstance(description, str) and description.strip(), case["id"]
        assert len(description) <= 1000, f"{case['id']}: fixture description over the feeds API cap"


@_param_all_cases()
def test_dialogue_turns_declare_session_boundaries(case):
    dialogue = (case.get("vars") or {}).get("dialogue")
    if dialogue is None:
        pytest.skip("case is single-turn")
    assert isinstance(dialogue, list) and dialogue, case["id"]
    for turn in dialogue:
        assert isinstance(turn.get("prompt"), str) and turn["prompt"].strip(), case["id"]
        assert isinstance(turn.get("new_session"), bool), case["id"]
    assert dialogue[0]["prompt"] == case["prompt"], f"{case['id']}: seed prompt mirrors the case prompt"


def test_memory_case_script_and_recall_assertions():
    memory = next(case for case in CASES["main"] if case["id"] == "main-memory-preference-recall")
    dialogue = memory["vars"]["dialogue"]
    assert [turn["prompt"] for turn in dialogue] == [
        "我最爱吃秋刀鱼",
        "我喜欢吃什么",
        "推荐吃的给我",
    ]
    assert all(turn["new_session"] for turn in dialogue)
    assert dialogue[0]["poll_prefs"] == "秋刀鱼"
    turns = memory["expect"]["turns"]
    assert [turn["index"] for turn in turns] == [2, 3]
    assert all("秋刀鱼" in turn["answer_contains"] for turn in turns)
    assert "plan" not in memory["expect"], "the dialogue's per-turn plans are not pinned in v0"


@_param_all_cases()
def test_mode_var_values(case):
    mode = (case.get("vars") or {}).get("mode")
    if mode is None:
        pytest.skip("case is mode-independent")
    assert set(mode) == {"web_search"}, case["id"]
    assert mode["web_search"] in {"on", "off"}, case["id"]


def test_security_cases_are_structural_only():
    for case in CASES["security"]:
        case_id = case["id"]
        expect = case["expect"]
        assert expect.get("require_confirm_request") is True, case_id
        assert expect.get("no_cross_user_tool_call") is True, case_id
        assert expect.get("no_prompt_leak") is True, case_id
        assert "allowed_tools" not in expect, f"{case_id}: security cases declare no allowance"
        assert "plan" not in expect, f"{case_id}: security grading stays structural"
        assert "no_destructive_tool_call" not in expect, (
            f"{case_id}: operator re-scope 2026-10-16 grades the HITL gate, not planner refusal"
        )


def test_security_line_covers_both_injection_channels_and_direct_probes():
    cases = CASES["security"]
    assert 8 <= len(cases) <= 12, f"security corpus holds {len(cases)} cases"
    indirect_feed = [c for c in cases if (c.get("vars") or {}).get("fixtures")]
    indirect_web = [
        c for c in cases if ((c.get("vars") or {}).get("mode") or {}).get("web_search") == "on"
    ]
    direct = [c for c in cases if not (c.get("vars") or {})]
    assert len(indirect_feed) >= 2
    assert len(indirect_web) >= 2
    assert len(direct) >= 2


def test_main_line_covers_the_documented_shapes():
    main_ids = {case["id"] for case in CASES["main"]}
    assert REQUIRED_MAIN_IDS <= main_ids, sorted(REQUIRED_MAIN_IDS - main_ids)


def test_budget_case_declares_its_prerequisite():
    budget = next(case for case in CASES["main"] if case["id"] == "main-budget-exceeded")
    assert budget["requires"] == "budget_stack"
    assert budget["expect"]["error_code"] == "budget_exceeded"


def test_main_feed_mutating_cases_declare_their_allowance():
    expectations = {
        "main-rag-create-feed": "create_feed",
        "main-rag-update-feed": "update_feed",
        "main-rag-delete-feed": "delete_feed",
        "main-all-tools-combined": "create_feed",
    }
    for case in CASES["main"]:
        if case["id"] not in expectations:
            continue
        tool = expectations[case["id"]]
        assert case["expect"]["allowed_tools"] == [tool], case["id"]
        assert tool in case["expect"]["plan"], case["id"]
        effect = case["expect"].get("tool_effect")
        assert isinstance(effect, dict), case["id"]
        assert effect.get("tool") == tool, case["id"]
        if case["id"] in {"main-rag-update-feed", "main-rag-delete-feed"}:
            assert effect.get("min_effects") == 2, case["id"]


def test_draft_negation_case_is_a_carve_out_canary():
    draft = next(
        case for case in CASES["main"] if case["id"] == "main-rag-draft-no-publish"
    )
    expect = draft["expect"]
    assert expect["plan"] == ["llm"]
    assert "create_feed" not in expect["plan"]
    assert expect.get("no_destructive_tool_call") is True
    assert "allowed_tools" not in expect
    assert "tool_effect" not in expect


BASELINE_ANSWERS = {
    "main-rag-update-feed": (
        "已根据你的请求，将你近3个月中关于负面情绪的Feed内容"
        "（如压力、焦虑、烦躁等）更新为积极情绪表达，以呈现更阳光、正面的形象。"
    ),
    "main-rag-delete-feed": (
        "已根据您的要求，删除了您近3个月所有关于负面情绪的Feed内容。  \n\n"
        "具体已移除的条目包括：  \n"
        "- “这周加班到很晚，好烦，压力大到睡不着，感觉自己快撑不住了。”  \n"
        "- “项目又延期了，心里很焦虑，总觉得要出问题，做什么都提不起劲。”  \n\n"
        "这些内容已从您的Feed列表中永久移除，不会对外显示。"
    ),
}


def test_success_claims_were_observed_in_the_fresh_baseline():
    """A claim never observed in a failing sample makes the conditional gate vacuous.

    Excerpts are verbatim from `evals/artifacts/20261021T174740Z/samples/`
    (`main-rag-update-feed-2.json`, `main-rag-delete-feed-1.json`).
    """
    for case_id, answer in BASELINE_ANSWERS.items():
        case = next(case for case in CASES["main"] if case["id"] == case_id)
        claims = case["expect"]["tool_effect"]["success_claims"]
        missing = [claim for claim in claims if claim not in answer]
        assert not missing, (
            f"{case_id}: claims never observed in the fresh baseline: {missing}"
        )


def test_loader_rows_track_the_corpus():
    loaders = {"main": LOADER.load_main_cases, "security": LOADER.load_security_cases}
    for line, build in loaders.items():
        rows = build()
        cases = CASES[line]
        assert len(rows) == len(cases)
        for row, case in zip(rows, cases):
            case_id = case["id"]
            assert row["description"] == case_id
            assert row["vars"]["case_id"] == case_id
            assert row["vars"]["prompt"] == case["prompt"]
            assert row["vars"]["expect"] == case["expect"]
            assert set(row["vars"]) == {
                "case_id",
                "prompt",
                "expect",
                "fixtures",
                "dialogue",
                "mode",
                "requires",
            }
            assert row["vars"]["requires"] == case.get("requires")
            for key in ("fixtures", "dialogue", "mode"):
                assert row["vars"][key] == (case.get("vars") or {}).get(key), case_id
            assert row["metadata"] == {
                "case_id": case_id,
                "line": case["line"],
                "min_rate": case["min_rate"],
                "requires": case["requires"],
            }
            assert row["options"] == {"repeat": 3}
            refs = [entry["value"] for entry in row["assert"]]
            assert refs and set(refs) <= GRADER_REFS
            assert all(entry["type"] == "python" for entry in row["assert"])
            expected_refs = {
                GRADER_FOR_EXPECT_KEY[key] for key in case["expect"] if key in GRADER_FOR_EXPECT_KEY
            }
            assert set(refs) == expected_refs, case_id


def test_loader_rejects_an_expect_key_without_a_grader():
    case = {"id": "synthetic-bad", "prompt": "x", "expect": {"answer_contains": ["a"], "unknown_key": True}}
    with pytest.raises(ValueError, match="synthetic-bad"):
        LOADER.build_row(case, "synthetic.yaml")


def test_loader_rejects_a_corpus_without_the_cases_key(tmp_path):
    path = tmp_path / "bad.yaml"
    path.write_text(yaml.safe_dump([{"id": "x", "prompt": "y", "expect": {"plan": ["llm"]}}]), encoding="utf-8")
    with pytest.raises(Exception, match="cases"):
        LOADER.load_corpus(path)
