import argparse
import fnmatch
import re
import subprocess
import sys

DIFF_EXTENSIONS = (".go", ".py", ".ts", ".tsx", ".js", ".jsx")

ALLOW = {
    ".go": (
        r"^// ?(go:|nolint|\+build|line )",
        r"^// (Copyright|SPDX-)",
        r"^// Code generated .* DO NOT EDIT\.",
    ),
    ".py": (
        r"^#!",
        r"^# -\*-",
        r"^# (noqa|type:|pragma:|fmt:|isort:|ruff:)",
    ),
    ".ts": (
        r"^///",
        r"^// ?(eslint|@ts-|prettier|@vitest-|#region|#endregion)",
        r"^/\* ?eslint",
    ),
}
ALLOW[".tsx"] = ALLOW[".ts"]
ALLOW[".js"] = ALLOW[".ts"]
ALLOW[".jsx"] = ALLOW[".ts"]

GENERATED_MARKERS = (
    "Code generated",
    "auto-generated",
    "DO NOT EDIT",
)

GENERATED_PATHS = (
    "frontend/src/api/openapi/schema.d.ts",
    "backend/internal/*/db/*.go",
)

HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@")
DIFF_FILE_RE = re.compile(r"^\+\+\+ (?:b/)?(.+)$")


def _generated(path: str) -> bool:
    if not any(fnmatch.fnmatch(path, pattern) for pattern in GENERATED_PATHS):
        return False
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            head = fh.read(4096)
    except OSError:
        return False
    return any(marker in head for marker in GENERATED_MARKERS)


def _ext(path: str) -> str:
    for ext in DIFF_EXTENSIONS:
        if path.endswith(ext):
            return ext
    return ""


def _comment_start(line: str, ext: str) -> bool:
    stripped = line.lstrip()
    if ext == ".py":
        return stripped.startswith("#")
    return stripped.startswith("//") or stripped.startswith("/*")


def _allowed(line: str, ext: str) -> bool:
    stripped = line.lstrip()
    return any(re.match(pat, stripped) for pat in ALLOW.get(ext, ()))


def iter_added_lines(diff_text: str):
    path = ""
    line_no = 0
    for raw in diff_text.splitlines():
        if raw.startswith("+++"):
            match = DIFF_FILE_RE.match(raw)
            if match:
                path = match.group(1).strip().strip('"')
            continue
        if raw.startswith("@@"):
            match = HUNK_RE.match(raw)
            line_no = int(match.group(1)) if match else 0
            continue
        if raw.startswith("+"):
            yield path, line_no, raw[1:]
            line_no += 1


def find_violations(diff_text: str) -> list[tuple[str, int, str]]:
    violations = []
    generated: dict[str, bool] = {}
    for path, line_no, text in iter_added_lines(diff_text):
        if path not in generated:
            generated[path] = _generated(path)
        if generated[path]:
            continue
        ext = _ext(path)
        if not ext or not _comment_start(text, ext):
            continue
        if _allowed(text, ext):
            continue
        violations.append((path, line_no, text.strip()))
    return violations


def _diff_command(args: argparse.Namespace) -> list[str]:
    cmd = ["git", "diff", "-U0", "--diff-filter=ACMR"]
    if args.range:
        cmd.append(args.range)
    else:
        cmd.append("--cached")
    cmd.append("--")
    cmd.extend(f"*{ext}" for ext in DIFF_EXTENSIONS)
    return cmd


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Fail when a diff adds explanatory code comments. "
            "Repo rule: AI coding should not write code comments — put the "
            "knowledge in a test, the commit message, or the docs instead. "
            "Tool directives are allowed (//go:, //nolint, build tags, "
            "# noqa, # type:, shebangs, // eslint-*, // @ts-*, ///). "
            "Scans added lines only, so every existing comment is grandfathered. "
            "Covers .go .py .ts .tsx .js .jsx; docstrings, shell, SQL, and "
            "block-comment continuation lines are out of scope. Generator "
            "output is exempt only where the path is on the generated list "
            "(frontend/src/api/openapi/schema.d.ts, backend/internal/*/db/*.go) "
            "AND the file carries a `Code generated` / `auto-generated` / "
            "`DO NOT EDIT` banner — a banner alone exempts nothing, so a file "
            "cannot exempt itself."
        )
    )
    parser.add_argument(
        "--range",
        metavar="REV",
        default="",
        help="git revision range to diff (e.g. origin/main...HEAD); default: --cached",
    )
    parser.add_argument(
        "--staged",
        action="store_true",
        help="check the staged diff (the default; kept for explicitness at call sites)",
    )
    args = parser.parse_args(argv)

    cmd = _diff_command(args)
    try:
        diff = subprocess.run(cmd, capture_output=True, text=True, check=True).stdout
    except FileNotFoundError:
        print("check_no_comments: git not found; skipping", file=sys.stderr)
        return 0
    except subprocess.CalledProcessError as exc:
        print(f"check_no_comments: git diff failed: {exc.stderr.strip()}", file=sys.stderr)
        return 2

    violations = find_violations(diff)
    if not violations:
        return 0

    print("check_no_comments: new code comments are not allowed:", file=sys.stderr)
    for path, line_no, text in violations:
        print(f"  {path}:{line_no}: {text}", file=sys.stderr)
    print(
        "Move the knowledge into a test assertion, the commit message, or the docs.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
