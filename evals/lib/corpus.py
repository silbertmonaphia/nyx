"""Golden-set corpus loader and load-time validation.

The corpus is operator-editable YAML data (`evals/cases/*.yaml`); this module
is the single gate every consumer reads it through, so a typo fails the run
before any model call — with the offending case id and field named
(agent-golden-eval: "unknown plan-step tokens, tool names not in the live
production tool catalog, or unknown frame kinds MUST fail the run").

Consumers: `evals/validate.py` (CLI gate) and `evals/bin/check_rate.py` (floor
lookup). Graders do NOT import this module — they read the `expect` block that
promptfoo hands them in `context["vars"]`.
"""

from __future__ import annotations

import glob
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

import yaml

TOOL_CATALOG = (
    "list_feeds",
    "get_feed",
    "create_feed",
    "update_feed",
    "delete_feed",
    "web_search",
    "web_fetch",
)
"""Tool names the harness accepts in a `plan` expectation.

Mirrors the production catalog: the Go MCP server's tool constants
(`backend/internal/mcp/model.go` — list_feeds, get_feed, create_feed,
update_feed, delete_feed) plus the agent's web tools (`web_search`,
`web_fetch`). This constant is the single place to extend when a tool lands;
the plan-token vocabulary derives from it. Every name here is a name a step
may legitimately carry, not a name the harness expects — a case still has to
spell its own expectation out.
"""

STEP_KINDS = ("rag", "llm", "react")
"""Plan-step kinds that normalize to a bare token.

`tool` is not a token: a tool step's token is the tool's name, so a
`tool`-kind step in an expectation would be un-gradeable.
"""

PLAN_TOKENS = frozenset(STEP_KINDS) | frozenset(TOOL_CATALOG)

FRAME_KINDS = frozenset(
    {
        "run_id",
        "plan",
        "step_start",
        "tool_call",
        "tool_result",
        "step_end",
        "delta",
        "note",
        "confirm_request",
        "done",
        "error",
        "terminator",
    }
)

KNOWN_LINES = ("main", "security")

REQUIRES_VALUES = ("budget_stack",)
"""Prerequisites a case may declare; anything else is a typo or an unimplemented stack."""

WEB_SEARCH_MODES = ("on", "off")
"""Web-search modes a case may key on.

Bare `on` / `off` are booleans under YAML 1.1 (PyYAML) but plain strings under
the YAML 1.2 core schema promptfoo's js-yaml implements, so `_normalize_modes`
maps the boolean reading back to its YAML-1.1 spelling — the corpus author
writes either form and both readers agree on the same bytes.
"""

ASSERTION_KEYS = frozenset(
    {
        "plan",
        "answer_contains",
        "answer_forbidden",
        "turns",
        "no_destructive_tool_call",
        "no_prompt_leak",
        "error_code",
        "require_confirm_request",
        "no_cross_user_tool_call",
        "payload_marker",
        "tool_effect",
    }
)

KNOWN_EXPECT_KEYS = ASSERTION_KEYS | {
    "allowed_tools",
    "payload_marker",
    "require_confirm_request",
    "no_cross_user_tool_call",
}

WRITE_EFFECTS = {
    "create_feed": "created",
    "update_feed": "updated",
    "delete_feed": "deleted",
}
"""Write tools a plan may name, and the execution effect each must produce."""

TOOL_EFFECT_KEYS = frozenset(
    {"tool", "effect", "step_index", "description_any_of", "success_claims"}
)

DIALOGUE_TURN_KEYS = frozenset({"prompt", "new_session", "poll_prefs"})

ID_PATTERN = re.compile(r"^[a-z0-9]+(-[a-z0-9]+)*$")

REPO_ROOT = Path(__file__).resolve().parents[2]


class CorpusError(Exception):
    """A corpus file that must not be executed."""

    def __init__(
        self,
        message: str,
        *,
        case_id: str | None = None,
        field_name: str | None = None,
        source: str | None = None,
    ) -> None:
        self.message = message
        self.case_id = case_id
        self.field_name = field_name
        self.source = source
        super().__init__(self.render())

    @property
    def field(self) -> str | None:
        """Alias for `field_name` (the corpus path of the offending value)."""
        return self.field_name

    def render(self) -> str:
        location = f" [{self.source}]" if self.source else ""
        subject = f"case {self.case_id!r}: " if self.case_id else ""
        where = f"{self.field_name}: " if self.field_name else ""
        return f"{subject}{where}{self.message}{location}"


@dataclass(frozen=True)
class Case:
    """One validated corpus entry."""

    id: str
    line: str
    prompt: str | None
    vars: Mapping[str, Any] = field(default_factory=dict)
    expect: Mapping[str, Any] = field(default_factory=dict)
    min_rate: float = 1.0
    requires: str | None = None
    source: str = ""
    position: int = 0

    @property
    def dialogue(self) -> Sequence[Mapping[str, Any]]:
        """The scripted dialogue when the case is multi-turn, else empty."""
        value = self.vars.get("dialogue")
        return value if isinstance(value, list) else []

    @property
    def mode(self) -> Mapping[str, Any]:
        value = self.vars.get("mode")
        return value if isinstance(value, dict) else {}


def resolve_patterns(patterns: Iterable[str], root: Path | None = None) -> list[str]:
    """Expand case globs, falling back to repo-root-relative resolution.

    A relative pattern is tried against the current directory first and then
    against the repo root, so `python evals/validate.py` behaves the same from
    the repo root and from inside `evals/`.
    """
    root = root or REPO_ROOT
    resolved: list[str] = []
    for pattern in patterns:
        matches = sorted(glob.glob(pattern, recursive=True))
        if not matches and not Path(pattern).is_absolute():
            matches = sorted(glob.glob(str(root / pattern), recursive=True))
        for match in matches:
            path = str(Path(match).resolve())
            if path not in resolved:
                resolved.append(path)
    return resolved


def load_corpus(patterns: Iterable[str] | str, *, root: Path | None = None) -> list[Case]:
    """Load and validate every case matched by `patterns`.

    Raises `CorpusError` naming the offending case id and field.
    """
    if isinstance(patterns, (str, Path)):
        patterns = [str(patterns)]
    patterns = [str(pattern) for pattern in patterns]

    files = resolve_patterns(patterns, root=root)
    if not files:
        raise CorpusError(
            f"no corpus files matched {patterns!r}",
            field_name="--cases-glob",
        )

    cases: list[Case] = []
    seen: dict[str, str] = {}
    for path in files:
        for case in _load_file(path):
            previous = seen.get(case.id)
            if previous is not None:
                raise CorpusError(
                    f"duplicate case id (also declared in {previous})",
                    case_id=case.id,
                    field_name="id",
                    source=path,
                )
            seen[case.id] = path
            cases.append(case)

    if not cases:
        raise CorpusError(
            f"no cases found in {files!r}",
            field_name="cases",
        )
    return cases


def corpus_by_id(cases: Iterable[Case]) -> dict[str, Case]:
    """Index loaded cases by id."""
    return {case.id: case for case in cases}


def _load_file(path: str) -> list[Case]:
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        raise CorpusError(f"cannot read corpus file ({exc})", source=path) from exc
    try:
        document = yaml.safe_load(text)
    except yaml.YAMLError as exc:
        raise CorpusError(f"not valid YAML ({exc})", source=path) from exc

    if document is None:
        raise CorpusError("empty corpus file", field_name="cases", source=path)
    if not isinstance(document, dict):
        raise CorpusError("top level must be a mapping", field_name="cases", source=path)
    raw_cases = document.get("cases")
    if raw_cases is None:
        raise CorpusError("missing 'cases' key", field_name="cases", source=path)
    if not isinstance(raw_cases, list):
        raise CorpusError("'cases' must be a list", field_name="cases", source=path)

    return [
        _parse_case(raw, source=path, position=position)
        for position, raw in enumerate(raw_cases)
    ]


def _error(message: str, case_id: str | None, field_name: str, source: str) -> CorpusError:
    return CorpusError(message, case_id=case_id, field_name=field_name, source=source)


def _normalize_modes(raw: dict) -> None:
    """Spell YAML-1.1 boolean mode readings (`on` / `off`) as the strings they are."""
    vars_ = raw.get("vars")
    if isinstance(vars_, dict):
        mode = vars_.get("mode")
        if isinstance(mode, dict) and isinstance(mode.get("web_search"), bool):
            mode["web_search"] = "on" if mode["web_search"] else "off"
    expect = raw.get("expect")
    plan = expect.get("plan") if isinstance(expect, dict) else None
    if isinstance(plan, dict):
        for key in [key for key in plan if isinstance(key, bool)]:
            name = "on" if key else "off"
            if name not in plan:
                plan[name] = plan.pop(key)


def _parse_case(raw: Any, *, source: str, position: int) -> Case:
    if not isinstance(raw, dict):
        raise _error(
            f"case #{position} must be a mapping, got {type(raw).__name__}",
            None,
            f"cases[{position}]",
            source,
        )
    _normalize_modes(raw)

    case_id = raw.get("id")
    if not isinstance(case_id, str) or not case_id:
        raise _error("missing case id", None, f"cases[{position}].id", source)
    if not ID_PATTERN.match(case_id):
        raise _error(
            f"case id {case_id!r} is not kebab-case (lowercase letters, digits, hyphens)",
            case_id,
            "id",
            source,
        )

    line = raw.get("line", "main")
    if line not in KNOWN_LINES:
        raise _error(f"unknown line {line!r} (known: {list(KNOWN_LINES)})", case_id, "line", source)

    vars_ = raw.get("vars") or {}
    if not isinstance(vars_, dict):
        raise _error(f"must be a mapping, got {type(vars_).__name__}", case_id, "vars", source)

    prompt = raw.get("prompt")
    dialogue = vars_.get("dialogue")
    if prompt is None:
        raise _error("prompt is required", case_id, "prompt", source)
    if not isinstance(prompt, str) or not prompt.strip():
        raise _error("must be a non-empty string", case_id, "prompt", source)
    if dialogue is not None:
        _validate_dialogue(dialogue, case_id, source)

    _validate_vars(vars_, case_id, source)

    expect = raw.get("expect")
    if expect is None:
        raise _error("is required", case_id, "expect", source)
    if not isinstance(expect, dict):
        raise _error(f"must be a mapping, got {type(expect).__name__}", case_id, "expect", source)
    _validate_expect(expect, case_id, source)

    min_rate = raw.get("min_rate")
    if min_rate is None:
        raise _error("is required", case_id, "min_rate", source)
    if isinstance(min_rate, bool) or not isinstance(min_rate, (int, float)):
        raise _error(
            f"must be a number in [0, 1], got {min_rate!r}", case_id, "min_rate", source
        )
    if not 0.0 <= float(min_rate) <= 1.0:
        raise _error(f"must be in [0, 1], got {min_rate!r}", case_id, "min_rate", source)

    requires = raw.get("requires")
    _validate_requires(requires, case_id, source)

    return Case(
        id=case_id,
        line=line,
        prompt=prompt,
        vars=vars_,
        expect=expect,
        min_rate=float(min_rate),
        requires=requires,
        source=source,
        position=position,
    )


def _validate_requires(requires: Any, case_id: str, source: str) -> None:
    if requires is None:
        return
    if not isinstance(requires, str) or requires not in REQUIRES_VALUES:
        raise _error(
            f"unknown prerequisite {requires!r} (known: {list(REQUIRES_VALUES)})",
            case_id,
            "requires",
            source,
        )


def _validate_dialogue(dialogue: Any, case_id: str, source: str) -> None:
    if not isinstance(dialogue, list) or not dialogue:
        raise _error(
            "must be a non-empty list of turns", case_id, "vars.dialogue", source
        )
    for index, turn in enumerate(dialogue):
        field_name = f"vars.dialogue[{index}]"
        if not isinstance(turn, dict):
            raise _error("must be a mapping", case_id, field_name, source)
        unknown = sorted(set(turn) - DIALOGUE_TURN_KEYS)
        if unknown:
            raise _error(
                f"unknown turn keys {unknown} (known: {sorted(DIALOGUE_TURN_KEYS)})",
                case_id,
                field_name,
                source,
            )
        prompt = turn.get("prompt")
        if not isinstance(prompt, str) or not prompt.strip():
            raise _error("must be a non-empty string", case_id, f"{field_name}.prompt", source)
        new_session = turn.get("new_session")
        if new_session is not None and not isinstance(new_session, bool):
            raise _error(
                f"must be a boolean, got {new_session!r}",
                case_id,
                f"{field_name}.new_session",
                source,
            )
        poll_prefs = turn.get("poll_prefs")
        if poll_prefs is not None and not isinstance(poll_prefs, (bool, str)):
            raise _error(
                f"must be true or a preference key string, got {poll_prefs!r}",
                case_id,
                f"{field_name}.poll_prefs",
                source,
            )


def _validate_vars(vars_: Mapping[str, Any], case_id: str, source: str) -> None:
    mode = vars_.get("mode")
    if mode is not None:
        if not isinstance(mode, dict):
            raise _error("must be a mapping", case_id, "vars.mode", source)
        web_search = mode.get("web_search")
        if web_search is not None and web_search not in WEB_SEARCH_MODES:
            raise _error(
                f"unknown web_search mode {web_search!r} (known: {list(WEB_SEARCH_MODES)})",
                case_id,
                "vars.mode.web_search",
                source,
            )

    fixtures = vars_.get("fixtures")
    if fixtures is None:
        return
    if not isinstance(fixtures, list):
        raise _error("must be a list", case_id, "vars.fixtures", source)
    for index, fixture in enumerate(fixtures):
        field_name = f"vars.fixtures[{index}]"
        if not isinstance(fixture, dict):
            raise _error("must be a mapping", case_id, field_name, source)
        for key in ("title", "description"):
            value = fixture.get(key)
            if not isinstance(value, str) or not value:
                raise _error(
                    "must be a non-empty string",
                    case_id,
                    f"{field_name}.{key}",
                    source,
                )


def _validate_expect(expect: Mapping[str, Any], case_id: str, source: str) -> None:
    for key in expect:
        if key not in KNOWN_EXPECT_KEYS:
            raise _error(
                f"unknown expectation field {key!r} (known: {sorted(KNOWN_EXPECT_KEYS)})",
                case_id,
                f"expect.{key}",
                source,
            )

    if not any(key in expect for key in ASSERTION_KEYS):
        raise _error(
            f"declares no assertion (expected one of {sorted(ASSERTION_KEYS)})",
            case_id,
            "expect",
            source,
        )

    if "plan" in expect:
        _validate_plan(expect["plan"], case_id, source)

    write_tokens = _write_tokens(expect.get("plan")) if "plan" in expect else []
    if "tool_effect" in expect:
        _validate_tool_effect(expect, case_id, source)
    elif write_tokens:
        _, token = write_tokens[0]
        raise _error(
            f"plan names write tool {token!r} but expect.tool_effect is missing; "
            "every write case must assert its execution effect",
            case_id,
            "expect.tool_effect",
            source,
        )

    for key in ("answer_contains", "answer_forbidden"):
        if key in expect:
            _string_list(expect[key], case_id, f"expect.{key}", source)

    if "allowed_tools" in expect:
        tools = _string_list(expect["allowed_tools"], case_id, "expect.allowed_tools", source)
        for tool in tools:
            if tool not in TOOL_CATALOG:
                raise _error(
                    f"unknown tool {tool!r} (catalog: {list(TOOL_CATALOG)})",
                    case_id,
                    "expect.allowed_tools",
                    source,
                )
        if expect.get("no_destructive_tool_call") is not True:
            raise _error(
                "allowed_tools only narrows no_destructive_tool_call; declare it too",
                case_id,
                "expect.allowed_tools",
                source,
            )

    if "payload_marker" in expect:
        marker = expect["payload_marker"]
        if not isinstance(marker, str) or not marker:
            raise _error("must be a non-empty string", case_id, "expect.payload_marker", source)

    for key in (
        "no_destructive_tool_call",
        "no_prompt_leak",
        "require_confirm_request",
        "no_cross_user_tool_call",
    ):
        if key in expect and not isinstance(expect[key], bool):
            raise _error(
                f"must be a boolean, got {expect[key]!r}", case_id, f"expect.{key}", source
            )

    if "error_code" in expect:
        code = expect["error_code"]
        if not isinstance(code, str) or not code:
            raise _error("must be a non-empty string", case_id, "expect.error_code", source)

    if "turns" in expect:
        _validate_expect_turns(expect["turns"], case_id, source)


def _validate_plan(plan: Any, case_id: str, source: str) -> None:
    if isinstance(plan, dict):
        if not plan:
            raise _error("mode matrix is empty", case_id, "expect.plan", source)
        for mode, tokens in plan.items():
            field_name = f"expect.plan.{mode}"
            if mode not in WEB_SEARCH_MODES:
                raise _error(
                    f"unknown mode key {mode!r} (known: {list(WEB_SEARCH_MODES)})",
                    case_id,
                    field_name,
                    source,
                )
            _validate_tokens(tokens, case_id, field_name, source)
        return
    _validate_tokens(plan, case_id, "expect.plan", source)


def _plan_variants(plan: Any) -> list[tuple[str, list[str]]]:
    if isinstance(plan, dict):
        return [
            (f"expect.plan.{mode}", [str(token) for token in tokens])
            for mode, tokens in plan.items()
        ]
    if isinstance(plan, list):
        return [("expect.plan", [str(token) for token in plan])]
    return []


def _write_tokens(plan: Any) -> list[tuple[str, str]]:
    hits: list[tuple[str, str]] = []
    for field_name, tokens in _plan_variants(plan):
        hits.extend(
            (field_name, token) for token in tokens if token in WRITE_EFFECTS
        )
    return hits


def _validate_tool_effect(
    expect: Mapping[str, Any], case_id: str, source: str
) -> None:
    spec = expect["tool_effect"]
    if not isinstance(spec, dict):
        raise _error(
            f"must be a mapping, got {type(spec).__name__}",
            case_id,
            "expect.tool_effect",
            source,
        )
    unknown = sorted(set(spec) - TOOL_EFFECT_KEYS)
    if unknown:
        raise _error(
            f"unknown keys {unknown} (known: {sorted(TOOL_EFFECT_KEYS)})",
            case_id,
            "expect.tool_effect",
            source,
        )

    tool = spec.get("tool")
    if tool not in WRITE_EFFECTS:
        raise _error(
            f"tool must be one of {sorted(WRITE_EFFECTS)}, got {tool!r}",
            case_id,
            "expect.tool_effect.tool",
            source,
        )
    effect = spec.get("effect")
    expected_effect = WRITE_EFFECTS[tool]
    if effect != expected_effect:
        raise _error(
            f"effect {effect!r} does not match tool {tool!r} "
            f"(expected {expected_effect!r})",
            case_id,
            "expect.tool_effect.effect",
            source,
        )

    step_index = spec.get("step_index")
    if step_index is not None and (
        isinstance(step_index, bool)
        or not isinstance(step_index, int)
        or step_index < 0
    ):
        raise _error(
            f"must be a non-negative integer, got {step_index!r}",
            case_id,
            "expect.tool_effect.step_index",
            source,
        )

    keywords = spec.get("description_any_of")
    if keywords is not None:
        values = _string_list(
            keywords,
            case_id,
            "expect.tool_effect.description_any_of",
            source,
        )
        if not values:
            raise _error(
                "must not be empty",
                case_id,
                "expect.tool_effect.description_any_of",
                source,
            )
        if any(not value.strip() for value in values):
            raise _error(
                "must not contain empty keywords",
                case_id,
                "expect.tool_effect.description_any_of",
                source,
            )
        if effect == "deleted":
            raise _error(
                "does not apply to a delete effect",
                case_id,
                "expect.tool_effect.description_any_of",
                source,
            )
    if effect == "created" and keywords is None:
        raise _error(
            "is required for a created effect",
            case_id,
            "expect.tool_effect.description_any_of",
            source,
        )

    claims = spec.get("success_claims")
    if claims is not None:
        values = _string_list(
            claims,
            case_id,
            "expect.tool_effect.success_claims",
            source,
        )
        if not values:
            raise _error(
                "must not be empty when declared",
                case_id,
                "expect.tool_effect.success_claims",
                source,
            )
        if any(not value.strip() for value in values):
            raise _error(
                "must not contain empty phrases",
                case_id,
                "expect.tool_effect.success_claims",
                source,
            )

    plan = expect.get("plan")
    if plan is None:
        raise _error(
            "requires expect.plan so the write step can be located",
            case_id,
            "expect.tool_effect",
            source,
        )
    for field_name, tokens in _plan_variants(plan):
        occurrences = [
            index for index, token in enumerate(tokens) if token == tool
        ]
        if step_index is not None:
            if step_index >= len(tokens):
                raise _error(
                    f"step_index {step_index} is out of range for {field_name} "
                    f"({len(tokens)} token(s))",
                    case_id,
                    "expect.tool_effect.step_index",
                    source,
                )
            if tokens[step_index] != tool:
                raise _error(
                    f"step_index {step_index} points at {tokens[step_index]!r} "
                    f"in {field_name}, not {tool!r}",
                    case_id,
                    "expect.tool_effect.step_index",
                    source,
                )
        else:
            if not occurrences:
                raise _error(
                    f"{field_name} names no {tool!r} step",
                    case_id,
                    "expect.tool_effect.tool",
                    source,
                )
            if len(occurrences) > 1:
                raise _error(
                    f"{field_name} names {tool!r} in {len(occurrences)} steps; "
                    "pin expect.tool_effect.step_index",
                    case_id,
                    "expect.tool_effect.step_index",
                    source,
                )
        other_writes = sorted(
            {
                token
                for token in tokens
                if token in WRITE_EFFECTS and token != tool
            }
        )
        if other_writes:
            raise _error(
                f"{field_name} names other write tool(s) {other_writes} with no "
                "execution assertion; one tool_effect per case",
                case_id,
                "expect.tool_effect.tool",
                source,
            )


def _validate_tokens(tokens: Any, case_id: str, field_name: str, source: str) -> None:
    values = _string_list(tokens, case_id, field_name, source)
    if not values:
        raise _error("must not be empty", case_id, field_name, source)
    for index, token in enumerate(values):
        if token not in PLAN_TOKENS:
            raise _error(
                f"unknown plan token {token!r} "
                f"(step kinds: {list(STEP_KINDS)}; tools: {list(TOOL_CATALOG)})",
                case_id,
                f"{field_name}[{index}]",
                source,
            )


def _validate_expect_turns(turns: Any, case_id: str, source: str) -> None:
    if not isinstance(turns, list) or not turns:
        raise _error("must be a non-empty list", case_id, "expect.turns", source)
    for index, entry in enumerate(turns):
        field_name = f"expect.turns[{index}]"
        if not isinstance(entry, dict):
            raise _error("must be a mapping", case_id, field_name, source)
        turn_index = entry.get("index")
        if isinstance(turn_index, bool) or not isinstance(turn_index, int) or turn_index < 1:
            raise _error(
                f"must be a positive integer, got {turn_index!r}",
                case_id,
                f"{field_name}.index",
                source,
            )
        for key in ("answer_contains", "answer_forbidden"):
            if key in entry:
                _string_list(entry[key], case_id, f"{field_name}.{key}", source)


def _string_list(value: Any, case_id: str, field_name: str, source: str) -> list[str]:
    if not isinstance(value, list):
        raise _error(
            f"must be a list, got {type(value).__name__}", case_id, field_name, source
        )
    for index, item in enumerate(value):
        if not isinstance(item, str):
            raise _error(
                f"must be a list of strings, got {type(item).__name__} at index {index}",
                case_id,
                field_name,
                source,
            )
    return list(value)
