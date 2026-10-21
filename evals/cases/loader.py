"""promptfoo test generator for the golden-set corpora.

promptfoo loads `cases/*.yaml` through this module (config `tests:` refs
`file://cases/loader.py:load_main_cases` / `:load_security_cases`) because a
corpus file keeps its cases under a top-level `cases:` key, which promptfoo
would otherwise read as a single test row. The generator reads the corpus,
maps each case's frozen fields into the promptfoo row shape, and attaches the
graders its `expect` block calls for:

- `plan` -> `graders/plan_shape.py` (may be a `{on, off}` mode map)
- `answer_contains` / `answer_forbidden` / `turns` -> `graders/answer.py`
- `no_destructive_tool_call` / `allowed_tools` / `no_prompt_leak` / `payload_marker`
  / `require_confirm_request` / `no_cross_user_tool_call` -> `graders/security.py`
- `tool_effect` -> `graders/tool_effect.py`
- `error_code` -> `graders/terminal.py`

Graders read the case through `context["vars"]`, so every row's vars carry
`case_id`, `prompt`, `expect`, `fixtures`, `dialogue` and `mode` (`None` when
the case does not set them). `min_rate`, `requires` and `line` ride in the row
metadata for the rate post-processor; they are floors and bookkeeping, not
grading input. Assert paths are resolved by promptfoo against the config's
directory (`evals/`), so `file://graders/<name>.py` is correct from any cwd.
"""

import os
import pathlib
import sys

import yaml

EVALS_DIR = pathlib.Path(__file__).resolve().parents[1]
if str(EVALS_DIR) not in sys.path:
    sys.path.insert(0, str(EVALS_DIR))

from lib import corpus as corpus_lib  # noqa: E402

REPEAT = int(os.environ.get("EVAL_REPEAT", "3"))

_MAIN_CORPUS = "main.yaml"
_SECURITY_CORPUS = "security.yaml"

_EXPECT_GRADERS = (
    ("plan", "plan_shape"),
    ("answer_contains", "answer"),
    ("answer_forbidden", "answer"),
    ("turns", "answer"),
    ("no_destructive_tool_call", "security"),
    ("allowed_tools", "security"),
    ("no_prompt_leak", "security"),
    ("payload_marker", "security"),
    ("require_confirm_request", "security"),
    ("no_cross_user_tool_call", "security"),
    ("tool_effect", "tool_effect"),
    ("error_code", "terminal"),
)

_VAR_KEYS = ("fixtures", "dialogue", "mode")


def load_corpus(path):
    """Read one corpus file and return its ordered case list.

    The file first goes through the shared validator (`lib.corpus`), so a
    promptfoo run that skips `evals/validate.py` still fails fast on a
    malformed corpus instead of grading garbage.
    """
    corpus_lib.load_corpus([str(path)])
    data = yaml.safe_load(pathlib.Path(path).read_text(encoding="utf-8"))
    if not isinstance(data, dict) or not isinstance(data.get("cases"), list):
        raise ValueError(f"{path}: corpus must be a mapping with a 'cases' list")
    return data["cases"]


def graders_for(case):
    """Grader file refs the case's expect block wires, in a stable order."""
    expect = case.get("expect") or {}
    names = []
    for key, grader in _EXPECT_GRADERS:
        if key in expect and grader not in names:
            names.append(grader)
    if not names:
        raise ValueError(f"{case.get('id')}: expect block wires no grader")
    return [f"file://graders/{name}.py" for name in names]


def build_row(case, corpus_path):
    """Map one corpus case to a promptfoo test row."""
    case_id = case["id"]
    case_vars = case.get("vars") or {}
    unknown = sorted(set(expect_key for expect_key in (case.get("expect") or {}) if expect_key not in dict(_EXPECT_GRADERS)))
    if unknown:
        raise ValueError(
            f"{case_id}: expect keys {unknown} in {corpus_path} have no grader wired"
        )
    return {
        "description": case_id,
        "vars": {
            "case_id": case_id,
            "prompt": case["prompt"],
            "expect": case.get("expect"),
            **{key: case_vars.get(key) for key in _VAR_KEYS},
            "requires": case.get("requires"),
        },
        "options": {"repeat": REPEAT},
        "metadata": {
            "case_id": case_id,
            "line": case.get("line"),
            "min_rate": case.get("min_rate"),
            "requires": case.get("requires"),
        },
        "assert": [{"type": "python", "value": ref} for ref in graders_for(case)],
    }


def build_rows(cases, corpus_path):
    return [build_row(case, corpus_path) for case in cases]


def _corpus_path(name):
    return pathlib.Path(__file__).resolve().with_name(name)


def load_main_cases(config=None):
    path = _corpus_path(_MAIN_CORPUS)
    return build_rows(load_corpus(path), path)


def load_security_cases(config=None):
    path = _corpus_path(_SECURITY_CORPUS)
    return build_rows(load_corpus(path), path)
