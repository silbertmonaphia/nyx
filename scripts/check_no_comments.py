import argparse
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

HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@")
DIFF_FILE_RE = re.compile(r"^\+\+\+ (?:b/)?(.+)$")


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
    for path, line_no, text in iter_added_lines(diff_text):
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
            "block-comment continuation lines are out of scope."
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
