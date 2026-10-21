"""promptfoo `exec:`-protocol provider for the Nyx agent golden-set eval.

One provider process is spawned per case invocation, so all
cross-invocation state lives on disk under `EVAL_ARTIFACTS_DIR`:
`eval_user.json` holds the per-run eval user's credentials + token and
`samples/<case_id>-<n>.{sse,json}` hold the raw wire captures.

Measured path (openspec/changes/agent-golden-eval design D3): the
provider calls the Python agent's production `POST /agent/run` directly,
with exactly the headers the Go proxy sets (`Authorization: Bearer
<jwt>`, `X-Nyx-User-Id: <id>`), and consumes the true SSE stream
(`event: <kind>` plus a flat JSON `data:` payload). The eval user is
registered/logged in through the Go API, which is what the MCP and RAG
bridges re-verify the bearer token against.

Mutating tool calls are gated fail-closed by the agent's approval gate,
so every `confirm_request` frame is answered immediately with `POST
/agent/approvals` (decision `approve`). Leaving one to its 300s TTL
would stall the run and skew the measured rate; the approval POST rides
the stream loop, so it lands while the run is paused mid-node.

Invocation contract (probed against promptfoo 0.124.1, the pinned
version — the `exec:` protocol does NOT use stdin):

    argv[1]  the rendered prompt string
    argv[2]  provider config JSON: `{"id": ..., "config": {...}, "env": {}}`
    argv[3]  context JSON: `{"vars": {...}, "prompt": {...}, "test": {...},
             "repeatIndex": <int>, "testIdx": <int>, ...}`; `vars` carries
             the case's corpus vars plus promptfoo's `__evalId` /
             `__evalStepId` / `__repeatIndex`
    stdin    empty

Standard output: the transcript JSON, verbatim — promptfoo hands the
provider's stdout to the grader as-is, so a `{"output": ...}` wrapper
would reach the grader instead of the transcript. Diagnostics go to
stderr only, and a failure exits non-zero so promptfoo marks the sample
an error instead of grading a missing transcript. For manual runs the
prompt may also arrive as `argv[1]` with no context, or as a JSON object
(`{"prompt", "vars"}`) on stdin; the recorder drives `execute_case`
directly.

Environment (names frozen by the Makefile integration owner):
    EVAL_GO_API_URL               Go API base, default http://localhost:8180
    EVAL_AGENT_URL                Python agent base, default http://localhost:9100
    EVAL_ARTIFACTS_DIR            artifact root (Makefile creates it)
    EVAL_RUN_STAMP                UTC stamp used in the eval-user email
    EVAL_DB_DSN                   prefs-poll DSN (default: dev stack)
    EVAL_WEB_SEARCH               on|off, informational only (the running
                                  stack owns the real flag; the weather case
                                  is the tripwire for a mismatch)
    EVAL_PREFS_TIMEOUT_SECONDS    extraction-landed bound, default 60

Transcript contract (frozen; the provider's graded output):
    {"status": "completed"|"error"|"skipped",
     "skip_reason": null|"<reason>",
     "turns": [{"prompt", "run_id", "session_id", "frames",
                "answer", "error"}],
     "meta": {"eval_user_id", "eval_user_email", "started_at",
              "finished_at"}}

`frames` is the ordered wire transcript, each entry `{"event": <kind>,
"data": {...}}` — the parser's view of the bytes a client sees, never an
in-process plan object. `answer` is the in-order concatenation of the
`delta` frames' `delta` field.
"""
from __future__ import annotations

import asyncio
import json
import os
import re
import sys
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Iterable, Iterator, Mapping, Protocol

import asyncpg
import httpx

REPO_ROOT = Path(__file__).resolve().parents[2]

DEFAULT_GO_API_URL = "http://localhost:8180"
DEFAULT_AGENT_URL = "http://localhost:9100"
DEFAULT_DB_DSN = (
    "postgresql://postgres:postgres@localhost:5433/nyx"
    "?options=-c%20search_path%3Dlanggraph%2Cagent"
)

EVAL_PASSWORD = "eval-golden-pass-1"
EVAL_EMAIL_TEMPLATE = "eval-golden-{stamp}@eval.nyx.local"
FEED_TITLE_PREFIX = "eval-"

REGISTER_PATH = "/api/register"
LOGIN_PATH = "/api/login"
FEEDS_PATH = "/api/feeds"
RUN_PATH = "/agent/run"
APPROVALS_PATH = "/agent/approvals"
GO_HEALTH_PATH = "/api/health"
AGENT_HEALTH_PATH = "/healthz"

PACE_SECONDS = 0.2
FEEDS_PAGE_SIZE = 100
FEEDS_SNAPSHOT_MAX_PAGES = 100
PREFLIGHT_TIMEOUT_SECONDS = 5.0
RUN_READ_TIMEOUT_SECONDS = 600.0
APPROVAL_TIMEOUT_SECONDS = 15.0
TOKEN_EXPIRY_MARGIN_SECONDS = 120.0
PREFS_POLL_INTERVAL_SECONDS = 1.0
PREFS_TIMEOUT_DEFAULT_SECONDS = 60.0

SKIP_PREFS_TIMEOUT = "preference extraction timeout"

FRAME_KINDS = frozenset(
    {
        "run_id",
        "note",
        "plan",
        "step_start",
        "tool_call",
        "tool_result",
        "step_end",
        "delta",
        "confirm_request",
        "done",
        "error",
        "terminator",
    }
)

SAMPLE_FILE_RE_TEMPLATE = r"^{case}-(\d+)\.(?:json|sse)$"


class ProviderError(RuntimeError):
    """A loud, operator-readable provider failure (no stack trace needed)."""

    def __init__(self, message: str, *, status: int | None = None) -> None:
        super().__init__(message)
        self.status = status


@dataclass(frozen=True)
class Frame:
    """One SSE frame as parsed from the wire: `event: <kind>` + flat `data:`."""

    event: str
    data: dict[str, Any]

    def as_dict(self) -> dict[str, Any]:
        return {"event": self.event, "data": self.data}


@dataclass(frozen=True)
class EvalUser:
    user_id: int
    email: str
    password: str
    access_token: str
    expires_at: float | None = None


@dataclass(frozen=True)
class Config:
    go_api_url: str
    agent_url: str
    artifacts_dir: Path
    run_stamp: str
    db_dsn: str
    web_search_mode: str
    prefs_timeout_seconds: float


class Transport(Protocol):
    """The HTTP seam; unit tests stub this instead of touching a socket."""

    def post_json(
        self,
        path: str,
        *,
        headers: dict[str, str],
        body: dict[str, Any],
        timeout: float,
    ) -> dict[str, Any]: ...

    def get_json(
        self,
        path: str,
        *,
        headers: dict[str, str],
        timeout: float,
    ) -> dict[str, Any]: ...

    def status(self, path: str, *, timeout: float) -> int: ...

    def stream_sse(
        self,
        path: str,
        *,
        headers: dict[str, str],
        body: dict[str, Any],
        timeout: float,
        on_frame: Callable[[Frame], None],
    ) -> str: ...


class PrefsReader(Protocol):
    """Read-only `agent.chat_user_prefs` probe used by the memory case."""

    def has_row(self, user_id: int, key: str | None) -> bool: ...


@dataclass
class Runtime:
    agent: Transport
    api: Transport
    prefs: PrefsReader
    sleeper: Callable[[float], None] = time.sleep
    monotonic: Callable[[], float] = time.monotonic
    clock: Callable[[], datetime] = lambda: datetime.now(timezone.utc)


def iter_sse_blocks(lines: Iterable[str]) -> Iterator[str]:
    """Split SSE lines into blocks, one per blank-line terminator.

    Comment-only blocks (keep-alive ticks) are yielded too, so a raw
    capture stays byte-faithful to what the client received; the caller
    decides which blocks carry a frame.
    """
    buffer: list[str] = []
    for line in lines:
        if line == "":
            if buffer:
                yield "\n".join(buffer)
                buffer = []
            continue
        buffer.append(line)
    if buffer:
        yield "\n".join(buffer)


def parse_sse_block(block: str) -> Frame | None:
    """Parse one SSE block into a Frame; `None` for comments / `[DONE]`.

    Unknown frame kinds are returned as-is — the transcript is a wire
    capture, not a filtered view. A `data:` payload that is not JSON
    lands as `{"_raw": <payload>}` so triage keeps the bytes the grader
    could not decode.
    """
    event: str | None = None
    data_lines: list[str] = []
    for line in block.splitlines():
        if not line or line.startswith(":"):
            continue
        field_name, separator, value = line.partition(":")
        if separator and value.startswith(" "):
            value = value[1:]
        if field_name == "event":
            event = value
        elif field_name == "data":
            data_lines.append(value)
    payload = "\n".join(data_lines)
    if event is None and (not data_lines or payload.strip() == "[DONE]"):
        return None
    if event is None:
        event = "message"
    if payload.strip() == "[DONE]":
        return None
    try:
        decoded = json.loads(payload) if payload else {}
    except json.JSONDecodeError:
        decoded = {"_raw": payload}
    if not isinstance(decoded, dict):
        decoded = {"_value": decoded}
    return Frame(event=event, data=decoded)


class HttpTransport:
    """Real HTTP transport; base URL injected so one client serves both hops."""

    def __init__(self, base_url: str, *, client: httpx.Client | None = None) -> None:
        self.base_url = base_url.rstrip("/")
        self._client = client or httpx.Client(
            timeout=httpx.Timeout(
                connect=10.0,
                read=RUN_READ_TIMEOUT_SECONDS,
                write=60.0,
                pool=60.0,
            )
        )

    def _url(self, path: str) -> str:
        return f"{self.base_url}{path}"

    def post_json(
        self,
        path: str,
        *,
        headers: dict[str, str],
        body: dict[str, Any],
        timeout: float,
    ) -> dict[str, Any]:
        url = self._url(path)
        try:
            response = self._client.post(url, headers=headers, json=body, timeout=timeout)
        except httpx.HTTPError as exc:
            raise ProviderError(f"POST {url} failed: {exc!r}") from exc
        if response.status_code >= 400:
            raise ProviderError(
                f"POST {url} -> HTTP {response.status_code}: {response.text[:300]}",
                status=response.status_code,
            )
        return _json_object(response, url)

    def get_json(
        self,
        path: str,
        *,
        headers: dict[str, str],
        timeout: float,
    ) -> dict[str, Any]:
        url = self._url(path)
        try:
            response = self._client.get(url, headers=headers, timeout=timeout)
        except httpx.HTTPError as exc:
            raise ProviderError(f"GET {url} failed: {exc!r}") from exc
        if response.status_code >= 400:
            raise ProviderError(
                f"GET {url} -> HTTP {response.status_code}: {response.text[:300]}",
                status=response.status_code,
            )
        return _json_object(response, url)

    def status(self, path: str, *, timeout: float) -> int:
        url = self._url(path)
        try:
            return self._client.get(url, timeout=timeout).status_code
        except httpx.HTTPError as exc:
            raise ProviderError(f"GET {url} failed: {exc!r}") from exc

    def stream_sse(
        self,
        path: str,
        *,
        headers: dict[str, str],
        body: dict[str, Any],
        timeout: float,
        on_frame: Callable[[Frame], None],
    ) -> str:
        url = self._url(path)
        blocks: list[str] = []
        try:
            with self._client.stream(
                "POST", url, headers=headers, json=body, timeout=timeout
            ) as response:
                if response.status_code >= 400:
                    response.read()
                    raise ProviderError(
                        f"POST {url} -> HTTP {response.status_code}: "
                        f"{response.text[:300]}",
                        status=response.status_code,
                    )
                for block in iter_sse_blocks(response.iter_lines()):
                    blocks.append(block)
                    frame = parse_sse_block(block)
                    if frame is not None:
                        on_frame(frame)
        except httpx.HTTPError as exc:
            raise ProviderError(f"POST {url} stream failed: {exc!r}") from exc
        return _join_blocks(blocks)


def _json_object(response: httpx.Response, url: str) -> dict[str, Any]:
    try:
        decoded = response.json()
    except ValueError as exc:
        raise ProviderError(
            f"{url} returned a non-JSON body: {response.text[:200]}"
        ) from exc
    if not isinstance(decoded, dict):
        raise ProviderError(f"{url} returned JSON that is not an object")
    return decoded


def _join_blocks(blocks: list[str]) -> str:
    if not blocks:
        return ""
    return "\n\n".join(blocks) + "\n\n"


class AsyncpgPrefsReader:
    """Read-only `agent.chat_user_prefs` probe over the dev-stack Postgres."""

    def __init__(self, dsn: str) -> None:
        self.dsn = dsn

    def has_row(self, user_id: int, key: str | None) -> bool:
        return asyncio.run(self._has_row(user_id, key))

    async def _has_row(self, user_id: int, key: str | None) -> bool:
        connection = await asyncpg.connect(self.dsn)
        try:
            if key is None:
                row = await connection.fetchrow(
                    "SELECT 1 FROM agent.chat_user_prefs "
                    "WHERE user_id = $1 LIMIT 1",
                    user_id,
                )
            else:
                row = await connection.fetchrow(
                    "SELECT 1 FROM agent.chat_user_prefs "
                    "WHERE user_id = $1 AND (key = $2 OR value::text LIKE '%' || $2 || '%') "
                    "LIMIT 1",
                    user_id,
                    key,
                )
            return row is not None
        finally:
            await connection.close()


def build_runtime(config: Config) -> Runtime:
    """Wire the production runtime; the single seam unit tests replace."""
    client = httpx.Client(
        timeout=httpx.Timeout(
            connect=10.0, read=RUN_READ_TIMEOUT_SECONDS, write=60.0, pool=60.0
        )
    )
    return Runtime(
        agent=HttpTransport(config.agent_url, client=client),
        api=HttpTransport(config.go_api_url, client=client),
        prefs=AsyncpgPrefsReader(config.db_dsn),
    )


def utc_stamp(now: datetime | None = None) -> str:
    moment = now or datetime.now(timezone.utc)
    return moment.strftime("%Y%m%dT%H%M%SZ")


def resolve_config(env: dict[str, str] | None = None) -> Config:
    """Build the run configuration from the frozen environment names.

    `EVAL_ARTIFACTS_DIR` and `EVAL_RUN_STAMP` are set by the Makefile;
    both have a soft fallback so the recording tool can be driven by
    hand. The stamp is persisted next to the artifacts so the eval-user
    email stays stable across the per-call provider processes.
    """
    source = os.environ if env is None else env
    stamp_env = (source.get("EVAL_RUN_STAMP") or "").strip()
    artifacts_env = (source.get("EVAL_ARTIFACTS_DIR") or "").strip()
    if artifacts_env:
        artifacts_dir = Path(artifacts_env).expanduser()
        if not artifacts_dir.is_absolute():
            artifacts_dir = REPO_ROOT / artifacts_dir
    elif stamp_env:
        artifacts_dir = REPO_ROOT / "evals" / "artifacts" / stamp_env
    else:
        artifacts_dir = REPO_ROOT / "evals" / "artifacts" / utc_stamp()
    run_stamp = stamp_env or _read_or_create_stamp(artifacts_dir)
    return Config(
        go_api_url=(source.get("EVAL_GO_API_URL") or DEFAULT_GO_API_URL).rstrip("/"),
        agent_url=(source.get("EVAL_AGENT_URL") or DEFAULT_AGENT_URL).rstrip("/"),
        artifacts_dir=artifacts_dir,
        run_stamp=run_stamp,
        db_dsn=source.get("EVAL_DB_DSN") or DEFAULT_DB_DSN,
        web_search_mode=source.get("EVAL_WEB_SEARCH") or "on",
        prefs_timeout_seconds=float(
            source.get("EVAL_PREFS_TIMEOUT_SECONDS") or PREFS_TIMEOUT_DEFAULT_SECONDS
        ),
    )


def _read_or_create_stamp(artifacts_dir: Path) -> str:
    stamp_path = artifacts_dir / "run_stamp.txt"
    try:
        existing = stamp_path.read_text(encoding="utf-8").strip()
    except OSError:
        existing = ""
    if existing:
        return existing
    stamp = utc_stamp()
    artifacts_dir.mkdir(parents=True, exist_ok=True)
    stamp_path.write_text(stamp + "\n", encoding="utf-8")
    return stamp


def preflight(config: Config, runtime: Runtime) -> None:
    """Fail loudly, naming the exact URL, when a dependency is unreachable."""
    checks = (
        ("Go API", runtime.api, GO_HEALTH_PATH, f"{config.go_api_url}{GO_HEALTH_PATH}"),
        (
            "agent",
            runtime.agent,
            AGENT_HEALTH_PATH,
            f"{config.agent_url}{AGENT_HEALTH_PATH}",
        ),
    )
    for label, transport, path, url in checks:
        try:
            status = transport.status(path, timeout=PREFLIGHT_TIMEOUT_SECONDS)
        except ProviderError as exc:
            raise ProviderError(
                f"preflight failed: {label} unreachable at {url} ({exc})"
            ) from exc
        if status >= 400:
            raise ProviderError(
                f"preflight failed: {label} at {url} returned HTTP {status}"
            )


def eval_user_email(config: Config) -> str:
    return EVAL_EMAIL_TEMPLATE.format(stamp=config.run_stamp)


def ensure_eval_user(config: Config, runtime: Runtime) -> EvalUser:
    """Register/login the per-run eval user, cached in `eval_user.json`."""
    email = eval_user_email(config)
    cache_path = config.artifacts_dir / "eval_user.json"
    cached = _read_user_cache(cache_path)
    if cached is not None and cached.email == email and _token_fresh(cached, runtime.clock()):
        return cached
    auth = _authenticate(runtime.api, email, EVAL_PASSWORD, runtime.sleeper)
    user = EvalUser(
        user_id=int(auth["user"]["id"]),
        email=email,
        password=EVAL_PASSWORD,
        access_token=str(auth["access_token"]),
        expires_at=_expiry_epoch(auth.get("expires_at")),
    )
    cache_path.parent.mkdir(parents=True, exist_ok=True)
    cache_path.write_text(
        json.dumps(
            {
                "user_id": user.user_id,
                "email": user.email,
                "password": user.password,
                "access_token": user.access_token,
                "expires_at": user.expires_at,
            },
            indent=2,
        ),
        encoding="utf-8",
    )
    cache_path.chmod(0o600)
    return user


def _authenticate(api: Transport, email: str, password: str, sleeper: Callable[[float], None]) -> dict[str, Any]:
    body = {"username": email, "password": password}
    try:
        sleeper(PACE_SECONDS)
        return api.post_json(
            LOGIN_PATH, headers={}, body=body, timeout=PREFLIGHT_TIMEOUT_SECONDS
        )
    except ProviderError as exc:
        if exc.status not in (401, 403, 404):
            raise
    try:
        sleeper(PACE_SECONDS)
        return api.post_json(
            REGISTER_PATH, headers={}, body=body, timeout=PREFLIGHT_TIMEOUT_SECONDS
        )
    except ProviderError as exc:
        if exc.status != 409:
            raise
        raise ProviderError(
            f"eval user {email} exists but login failed with "
            f"{exc.status}; the cached credentials in eval_user.json are stale"
        ) from exc


def _read_user_cache(path: Path) -> EvalUser | None:
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    try:
        return EvalUser(
            user_id=int(raw["user_id"]),
            email=str(raw["email"]),
            password=str(raw["password"]),
            access_token=str(raw.get("access_token") or ""),
            expires_at=(
                float(raw["expires_at"]) if raw.get("expires_at") is not None else None
            ),
        )
    except (KeyError, TypeError, ValueError):
        return None


def _token_fresh(user: EvalUser, now: datetime) -> bool:
    if not user.access_token:
        return False
    if user.expires_at is None:
        return True
    return user.expires_at - TOKEN_EXPIRY_MARGIN_SECONDS > now.timestamp()


def _expiry_epoch(raw: Any) -> float | None:
    if not isinstance(raw, str) or not raw:
        return None
    try:
        parsed = datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.timestamp()


def auth_headers(user: EvalUser) -> dict[str, str]:
    return {
        "Authorization": f"Bearer {user.access_token}",
        "X-Nyx-User-Id": str(user.user_id),
    }


def normalize_feed_title(title: str) -> str:
    stripped = title.strip()
    if stripped.startswith(FEED_TITLE_PREFIX):
        return stripped
    return f"{FEED_TITLE_PREFIX}{stripped}"


def fixture_entries(fixtures: Any) -> list[dict[str, str]]:
    """Normalize a case's `fixtures` var to `{title, description}` dicts."""
    if not fixtures:
        return []
    if not isinstance(fixtures, (list, tuple)):
        raise ProviderError(
            f"vars.fixtures must be a list, got {type(fixtures).__name__}"
        )
    entries: list[dict[str, str]] = []
    for item in fixtures:
        if isinstance(item, str):
            entries.append({"title": normalize_feed_title(item), "description": ""})
        elif isinstance(item, dict):
            title = normalize_feed_title(str(item.get("title") or ""))
            entries.append(
                {"title": title, "description": str(item.get("description") or "")}
            )
        else:
            raise ProviderError(
                f"vars.fixtures entries must be a string or a mapping, got {item!r}"
            )
    return entries


def seed_fixtures(
    runtime: Runtime,
    user: EvalUser,
    fixtures: Any,
) -> list[dict[str, str]]:
    """Create the case's feed fixtures for the eval user, idempotently.

    Runs before the prompt: a RAG case must find its content already
    indexed. Writes are paced (~200ms) — the Go API rate-limits 10 rps
    per IP, and an unpaced burst of creates would 429.
    """
    entries = fixture_entries(fixtures)
    if not entries:
        return []
    existing = _existing_feed_titles(runtime, user)
    created: list[dict[str, str]] = []
    for entry in entries:
        if entry["title"] in existing:
            created.append(entry)
            continue
        runtime.sleeper(PACE_SECONDS)
        runtime.api.post_json(
            FEEDS_PATH,
            headers=auth_headers(user),
            body={"title": entry["title"], "description": entry["description"]},
            timeout=30.0,
        )
        created.append(entry)
    return created


def _existing_feed_titles(runtime: Runtime, user: EvalUser) -> set[str]:
    payload = runtime.api.get_json(
        f"{FEEDS_PATH}?page=1&page_size={100}",
        headers=auth_headers(user),
        timeout=30.0,
    )
    titles: set[str] = set()
    for row in payload.get("data") or []:
        if isinstance(row, dict) and row.get("title"):
            titles.add(str(row["title"]))
    return titles


def tool_effect_declared(variables: Mapping[str, Any]) -> bool:
    """True when the case declares `expect.tool_effect` (the snapshot opt-in)."""
    expect = variables.get("expect")
    return isinstance(expect, dict) and isinstance(expect.get("tool_effect"), dict)


def snapshot_feeds(
    runtime: Runtime, user: EvalUser
) -> tuple[list[dict[str, Any]] | None, str | None]:
    """All of the eval user's feeds, following `has_more` pagination.

    Returns `(rows, None)` on success and `(None, error)` on any failure;
    snapshot failures are recorded, never raised, so a payload-only
    assertion can still grade the sample.
    """
    rows: list[dict[str, Any]] = []
    page = 1
    while page <= FEEDS_SNAPSHOT_MAX_PAGES:
        path = f"{FEEDS_PATH}?page={page}&page_size={FEEDS_PAGE_SIZE}"
        try:
            payload = runtime.api.get_json(
                path, headers=auth_headers(user), timeout=30.0
            )
        except ProviderError as exc:
            return None, f"page {page}: {exc}"
        data = payload.get("data")
        if not isinstance(data, list):
            return None, f"page {page}: response carries no data list"
        rows.extend(row for row in data if isinstance(row, dict))
        if payload.get("has_more") is not True:
            return rows, None
        page += 1
    return None, f"pagination exceeded {FEEDS_SNAPSHOT_MAX_PAGES} pages"


def assemble_answer(frames: list[Frame]) -> str:
    """Concatenate the `delta` frames' text in wire order."""
    parts: list[str] = []
    for frame in frames:
        if frame.event != "delta":
            continue
        text = frame.data.get("delta")
        if isinstance(text, str):
            parts.append(text)
    return "".join(parts)


def turn_error(frames: list[Frame]) -> dict[str, str] | None:
    """Per-turn error from an `error` frame, or an unfinished stream."""
    for frame in frames:
        if frame.event == "error":
            code = frame.data.get("code")
            message = frame.data.get("error")
            return {
                "code": str(code) if code else "error",
                "message": str(message) if message else "",
            }
    if not any(frame.event == "done" for frame in frames):
        return {
            "code": "stream_incomplete",
            "message": "the SSE stream ended without a done or error frame",
        }
    return None


def turn_script(request: dict[str, Any], variables: dict[str, Any]) -> list[dict[str, Any]]:
    """The case's turns: `vars.dialogue` when present, else the single prompt."""
    dialogue = variables.get("dialogue")
    if dialogue:
        if not isinstance(dialogue, (list, tuple)):
            raise ProviderError(
                f"vars.dialogue must be a list, got {type(dialogue).__name__}"
            )
        turns = []
        for index, entry in enumerate(dialogue):
            if not isinstance(entry, dict) or not entry.get("prompt"):
                raise ProviderError(f"vars.dialogue[{index}] needs a prompt field")
            turns.append(
                {
                    "prompt": str(entry["prompt"]),
                    "new_session": bool(
                        entry.get("new_session", index == 0)
                    ),
                    "poll_prefs": entry.get("poll_prefs"),
                }
            )
        return turns
    prompt = request.get("prompt")
    if not isinstance(prompt, str) or not prompt.strip():
        raise ProviderError("the promptfoo request carries no prompt")
    return [{"prompt": prompt, "new_session": True, "poll_prefs": None}]


def case_id_of(request: dict[str, Any], variables: dict[str, Any]) -> str:
    candidate = variables.get("case_id")
    if candidate:
        return _safe_slug(str(candidate))
    test = request.get("test")
    if isinstance(test, dict):
        nested = (test.get("vars") or {}).get("case_id")
        if nested:
            return _safe_slug(str(nested))
    return "unnamed-case"


def _safe_slug(value: str) -> str:
    slug = re.sub(r"[^A-Za-z0-9._-]+", "-", value.strip()).strip("-")
    return slug or "unnamed-case"


def poll_key(poll_prefs: Any) -> str | None:
    """Optional `key` filter for the prefs poll (bare string or `{key: ...}`)."""
    if isinstance(poll_prefs, dict):
        key = poll_prefs.get("key")
        return str(key) if key else None
    if isinstance(poll_prefs, str):
        return poll_prefs
    return None


def wait_for_pref_row(
    reader: PrefsReader,
    user_id: int,
    key: str | None,
    *,
    timeout: float,
    interval: float,
    sleeper: Callable[[float], None],
    monotonic: Callable[[], float],
) -> bool:
    """Bounded poll for the extraction write. Returns False on timeout."""
    deadline = monotonic() + timeout
    while True:
        if reader.has_row(user_id, key):
            return True
        if monotonic() >= deadline:
            return False
        sleeper(interval)


def next_sample_index(artifacts_dir: Path, case_id: str) -> int:
    """1-based invocation counter for the case, derived from disk state."""
    samples_dir = artifacts_dir / "samples"
    pattern = re.compile(
        SAMPLE_FILE_RE_TEMPLATE.format(case=re.escape(case_id))
    )
    highest = 0
    try:
        names = [entry.name for entry in samples_dir.iterdir()]
    except OSError:
        return 1
    for name in names:
        match = pattern.match(name)
        if match:
            highest = max(highest, int(match.group(1)))
    return highest + 1


def empty_transcript(user: EvalUser, started_at: str, finished_at: str) -> dict[str, Any]:
    return {
        "status": "completed",
        "skip_reason": None,
        "turns": [],
        "meta": {
            "eval_user_id": user.user_id,
            "eval_user_email": user.email,
            "started_at": started_at,
            "finished_at": finished_at,
        },
    }


def skipped_transcript(case_id: str, reason: str, runtime: Runtime) -> dict[str, Any]:
    stamp = runtime.clock().isoformat()
    return {
        "status": "skipped",
        "skip_reason": reason,
        "turns": [],
        "meta": {
            "case_id": case_id,
            "eval_user_id": None,
            "eval_user_email": None,
            "started_at": stamp,
            "finished_at": stamp,
        },
    }


def missing_prerequisite(requires: str, env: Mapping[str, str]) -> str | None:
    """Machine-readable SKIP reason when a declared prerequisite is absent."""
    if requires == "budget_stack":
        if env.get("EVAL_BUDGET_STACK") == "1":
            return None
        return (
            "requires budget_stack (start a small-budget agent stack with "
            "AGENT_BUDGET_PER_USER_PER_HOUR tiny and set EVAL_BUDGET_STACK=1)"
        )
    return f"unknown prerequisite {requires!r}"


def run_turn(
    runtime: Runtime,
    user: EvalUser,
    prompt: str,
    *,
    echo_run_id: str | None,
) -> tuple[list[Frame], str, str, str | None]:
    """Run one production turn, approving gates as they appear.

    Returns `(frames, run_id, session_id, error_code)`. The approval POST
    happens inside the frame callback, so it lands while the run is
    paused on the gate rather than after the stream closes.
    """
    frames: list[Frame] = []
    seen_run_id: dict[str, str | None] = {"value": echo_run_id}
    approval_errors: list[str] = []

    def on_frame(frame: Frame) -> None:
        frames.append(frame)
        if frame.event == "run_id":
            value = frame.data.get("run_id")
            if isinstance(value, str) and value:
                seen_run_id["value"] = value
        elif frame.event == "confirm_request":
            try:
                approve(runtime, user, frame, echo_run_id)
            except ProviderError as exc:
                approval_errors.append(str(exc))

    body: dict[str, Any] = {"messages": [{"role": "user", "content": prompt}]}
    if echo_run_id:
        body["run_id"] = echo_run_id
    raw_sse = runtime.agent.stream_sse(
        RUN_PATH,
        headers=auth_headers(user),
        body=body,
        timeout=RUN_READ_TIMEOUT_SECONDS,
        on_frame=on_frame,
    )
    if approval_errors:
        raise ProviderError(
            "approval decision failed while the run was paused: "
            + "; ".join(approval_errors)
        )
    run_id = seen_run_id["value"] or ""
    return frames, run_id, run_id, raw_sse


def approve(
    runtime: Runtime,
    user: EvalUser,
    frame: Frame,
    fallback_run_id: str | None,
) -> None:
    """Answer a `confirm_request` immediately (mutating calls fail closed)."""
    run_id = frame.data.get("run_id") or fallback_run_id
    approval_id = frame.data.get("approval_id")
    if not run_id or not approval_id:
        raise ProviderError(
            f"confirm_request frame is missing run_id/approval_id: {frame.data}"
        )
    runtime.agent.post_json(
        APPROVALS_PATH,
        headers=auth_headers(user),
        body={
            "run_id": str(run_id),
            "approval_id": str(approval_id),
            "decision": "approve",
        },
        timeout=APPROVAL_TIMEOUT_SECONDS,
    )


def execute_case(
    request: dict[str, Any],
    *,
    config: Config,
    runtime: Runtime,
) -> dict[str, Any]:
    """Run one case invocation and return its transcript."""
    variables = request.get("vars") or {}
    if not isinstance(variables, dict):
        raise ProviderError("the promptfoo request's vars must be an object")
    case_id = case_id_of(request, variables)
    requires = variables.get("requires")
    if isinstance(requires, str) and requires:
        absent = missing_prerequisite(requires, os.environ)
        if absent is not None:
            return skipped_transcript(case_id, absent, runtime)
    turns = turn_script(request, variables)
    user = ensure_eval_user(config, runtime)
    seed_fixtures(runtime, user, variables.get("fixtures"))

    snapshot_requested = tool_effect_declared(variables)
    snapshot_errors: list[str] = []
    started_at = runtime.clock().isoformat()
    transcript = empty_transcript(user, started_at, started_at)
    if snapshot_requested:
        rows, error = snapshot_feeds(runtime, user)
        if rows is not None:
            transcript["meta"]["feeds_before"] = rows
        if error is not None:
            snapshot_errors.append(f"before: {error}")
    sample_blocks: list[str] = []
    sample_index = next_sample_index(config.artifacts_dir, case_id)
    previous_run_id: str | None = None

    for index, turn in enumerate(turns):
        echo = None if turn["new_session"] else previous_run_id
        frames, run_id, session_id, raw = run_turn(
            runtime, user, turn["prompt"], echo_run_id=echo
        )
        previous_run_id = run_id or previous_run_id
        if sample_blocks:
            sample_blocks.append(f": --- turn {index + 1} ---\n\n")
        sample_blocks.append(raw)
        turn_error_value = turn_error(frames)
        transcript["turns"].append(
            {
                "prompt": turn["prompt"],
                "run_id": run_id,
                "session_id": session_id,
                "frames": [frame.as_dict() for frame in frames],
                "answer": assemble_answer(frames),
                "error": turn_error_value,
            }
        )
        if turn_error_value is not None:
            transcript["status"] = "error"
            break
        poll_requested = bool(turn.get("poll_prefs")) or (
            index == 0 and bool(variables.get("poll_prefs"))
        )
        if poll_requested and not wait_for_pref_row(
            runtime.prefs,
            user.user_id,
            poll_key(turn.get("poll_prefs") or variables.get("poll_prefs")),
            timeout=config.prefs_timeout_seconds,
            interval=PREFS_POLL_INTERVAL_SECONDS,
            sleeper=runtime.sleeper,
            monotonic=runtime.monotonic,
        ):
            transcript["status"] = "skipped"
            transcript["skip_reason"] = SKIP_PREFS_TIMEOUT
            break

    if snapshot_requested:
        rows, error = snapshot_feeds(runtime, user)
        if rows is not None:
            transcript["meta"]["feeds_after"] = rows
        if error is not None:
            snapshot_errors.append(f"after: {error}")
        if snapshot_errors:
            transcript["meta"]["feeds_snapshot_error"] = "; ".join(snapshot_errors)

    transcript["meta"]["finished_at"] = runtime.clock().isoformat()
    write_samples(config, case_id, sample_index, sample_blocks, transcript)
    return transcript


def write_samples(
    config: Config,
    case_id: str,
    index: int,
    sample_blocks: list[str],
    transcript: dict[str, Any],
) -> None:
    """Dump the raw wire capture and the transcript for one invocation.

    All turns of a dialogue share the invocation's counter, so the raw
    blocks are concatenated in turn order (a parser-legal SSE comment
    separates turns) — every turn's bytes are still in the artifact.
    """
    samples_dir = config.artifacts_dir / "samples"
    samples_dir.mkdir(parents=True, exist_ok=True)
    (samples_dir / f"{case_id}-{index}.sse").write_text(
        "".join(sample_blocks), encoding="utf-8"
    )
    (samples_dir / f"{case_id}-{index}.json").write_text(
        json.dumps(transcript, ensure_ascii=False, indent=2), encoding="utf-8"
    )


def render_transcript(transcript: dict[str, Any]) -> str:
    """The provider's stdout: the transcript JSON, with no wrapper.

    promptfoo passes an `exec:` provider's stdout through verbatim, so
    the grader receives exactly this string.
    """
    return json.dumps(transcript, ensure_ascii=False)


def parse_invocation(argv: list[str], stdin_text: str) -> dict[str, Any]:
    """Build the internal request from the promptfoo exec invocation.

    `argv` is `sys.argv[1:]`. promptfoo supplies prompt + context as
    argv[1] and argv[3] and nothing on stdin; the argv[1]-only and stdin
    forms exist so the provider can also be driven by hand.
    """
    if len(argv) >= 3:
        context = _try_json(argv[2])
        if isinstance(context, dict) and "vars" in context:
            return {
                "prompt": argv[0],
                "vars": context.get("vars") or {},
                "test": context.get("test") or {},
                "context": context,
            }
    if stdin_text.strip():
        request = _try_json(stdin_text)
        if not isinstance(request, dict):
            raise ProviderError("stdin is not a JSON object")
        return request
    if argv and argv[0].strip():
        return {"prompt": argv[0], "vars": {}}
    raise ProviderError(
        "no prompt received: expected argv[1] (prompt) plus argv[3] "
        "(context), or a JSON request on stdin"
    )


def _try_json(text: str) -> Any:
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        return None


def main() -> int:
    try:
        request = parse_invocation(sys.argv[1:], sys.stdin.read())
    except ProviderError as exc:
        print(f"agent_run provider: {exc}", file=sys.stderr)
        return 2
    try:
        config = resolve_config()
        runtime = build_runtime(config)
        preflight(config, runtime)
        transcript = execute_case(request, config=config, runtime=runtime)
    except ProviderError as exc:
        print(f"agent_run provider: {exc}", file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001 — the exec protocol needs a loud exit
        print(f"agent_run provider: unexpected failure: {exc!r}", file=sys.stderr)
        return 1
    print(render_transcript(transcript))
    return 0


if __name__ == "__main__":
    sys.exit(main())
