"""User-preference memory for the planner (openspec/changes/agent-user-preferences).

Slice 3 of `AgentMemoryRoadmap.md`: remember stable preferences the user
states in conversation and feed them into the planner's system prompt so
the agent stops treating every session like a stranger. Four concerns
live here:

  - `PrefsStore` — thin async wrapper over `agent.chat_user_prefs` (same
    pool + `_with_retry` posture as `ChatStore`). One row per
    `(user_id, key)`: writing a preference upserts in place
    (`ON CONFLICT (user_id, key) DO UPDATE`), so "what do we believe
    today" is a plain `ORDER BY updated_at DESC LIMIT N` read with no
    read-time reduction (design.md D1).
  - `looks_like_preference` — the cheap gate. Extraction is only worth an
    LLM call when the message carries a first-person preference signal
    (design.md D3). Deliberately simple: false negatives (a missed
    preference) are recoverable; false positives cost one LLM call.
  - `extract_preferences` — one small LLM call with a JSON-output prompt,
    Pydantic-validated. Fire-and-forget by contract: any failure (client
    construction, LLM error, malformed output) logs a WARNING and returns
    `[]` — never raises, because the caller runs it as an `asyncio` task
    off the reply path (proposal.md "fire-and-forget").
  - `prefs_block` — renders the planner prompt block. Returns `""` for an
    empty list so the planner system prompt stays byte-identical to its
    pre-injection form (design.md D5 parity contract).

Lifecycle: rows are derived from a session and die with it — the chat
retention cron hard-deletes a session's prefs in the same iteration via
`delete_prefs_for_sessions` (design.md D6); `delete_prefs_for_user` is
the user-scoped erasure hook (slice 4).
"""
from __future__ import annotations

import asyncio
import json as _json
import logging
import re
from dataclasses import dataclass
from datetime import datetime
from typing import Any

import asyncpg
from langchain_core.messages import HumanMessage, SystemMessage
from langchain_openai import ChatOpenAI
from pydantic import BaseModel, SecretStr

from .config import Settings

log = logging.getLogger("nyx.agent.prefs")


class PrefsStoreUnavailable(RuntimeError):
    """Raised when prefs-store operations fail after the retry budget is exhausted."""


@dataclass(slots=True)
class Pref:
    """One user preference. `value` is the JSON-decoded Python object.

    `confidence` is an extraction-time signal (0..1) and is NOT persisted
    — the table keeps one current truth per key with no confidence column
    (design.md D1) — so rows loaded from the store always carry the
    default 1.0. `source_session_id` is the session the preference was
    stated in (None when the writer had no session row); `updated_at` is
    the store's write timestamp (None for pre-upsert/LLM-only values).
    """

    key: str
    value: Any
    confidence: float = 1.0
    source_session_id: str | None = None
    updated_at: datetime | None = None


class PrefsStore:
    """Thin async wrapper over `agent.chat_user_prefs`.

    Connection pool owned by the caller (reused from the shared asyncpg
    pool — no second pool, same posture as `ChatStore`).
    """

    def __init__(
        self,
        pool: asyncpg.Pool,
        *,
        max_retries: int = 3,
        acquire_timeout: float = 5.0,
    ) -> None:
        self._pool = pool
        self._max_retries = max_retries
        self._acquire_timeout = acquire_timeout

    async def _with_retry(self, op_name: str, fn):
        """Run `fn(conn)` with retry/backoff for transient asyncpg errors.

        Identical semantics to `ChatStore._with_retry`: pool-acquire
        timeout raises `PrefsStoreUnavailable` immediately (retrying a
        saturated pool within the same request won't help); transient
        `asyncpg.PostgresConnectionError` retries with 100ms→200ms→400ms
        exponential backoff up to `max_retries`.
        """
        attempt = 0
        backoff_ms = 100
        while True:
            try:
                async with self._pool.acquire(timeout=self._acquire_timeout) as conn:
                    return await fn(conn)
            # Builtin `TimeoutError` — `asyncio.TimeoutError` is an alias of
            # it since 3.11 (same class, so the pool-acquire timeout is
            # caught identically).
            except TimeoutError as exc:
                log.warning(
                    "prefs_store %s: pool acquire timeout after %.1fs (%r)",
                    op_name,
                    self._acquire_timeout,
                    exc,
                )
                raise PrefsStoreUnavailable(
                    f"{op_name} failed: pool acquire timeout "
                    f"({self._acquire_timeout}s)"
                ) from exc
            except asyncpg.PostgresConnectionError as exc:
                attempt += 1
                if attempt > self._max_retries:
                    log.warning(
                        "prefs_store %s: retries exhausted (%r)", op_name, exc
                    )
                    raise PrefsStoreUnavailable(
                        f"{op_name} failed after {self._max_retries} retries"
                    ) from exc
                log.info(
                    "prefs_store %s: retry %d/%d after %dms (%r)",
                    op_name,
                    attempt,
                    self._max_retries,
                    backoff_ms,
                    exc,
                )
                await asyncio.sleep(backoff_ms / 1000)
                backoff_ms *= 2

    async def upsert_pref(
        self,
        user_id: int,
        key: str,
        value: Any,
        source_session_id: str | None = None,
    ) -> None:
        """Insert or overwrite the `(user_id, key)` preference row.

        Upsert-in-place is the whole model (design.md D1): the UNIQUE
        index on `(user_id, key)` (alembic 0005) is the `ON CONFLICT`
        target, and an existing row's `value`, `source_session_id`, and
        `updated_at` are overwritten — one row per key per user, always
        the latest statement. `created_at` is left alone.

        `value` is serialized with `json.dumps` and cast (`$3::jsonb`):
        any JSON type is valid (string, number, array, object, null).
        `source_session_id` may be None — a NULL-sourced pref is skipped
        by the retention cleanup (no id to name it) and follows the
        user-scoped erasure path instead (design.md D6).

        **Erasure durability (review 2026-10-14, H2):** when a
        `source_session_id` is given, the write is skipped unless that
        session row still exists. The column deliberately has no FK
        (D1 — NULL-source + retention decoupling), so without this guard
        an extraction task racing an erasure (or a retention
        hard-delete) could recreate a prefs row for a just-erased
        subject pointing at a deleted session. The guard is expressed in
        the SQL (`INSERT ... SELECT ... WHERE EXISTS`), so it cannot be
        raced between a check and the write.
        """
        value_json = _json.dumps(value)

        async def _insert(conn: asyncpg.Connection) -> None:
            await conn.execute(
                "INSERT INTO chat_user_prefs "
                "(user_id, key, value, source_session_id) "
                "SELECT $1, $2, $3::jsonb, $4::uuid "
                "WHERE $4::uuid IS NULL OR EXISTS ("
                "  SELECT 1 FROM chat_sessions s WHERE s.id = $4::uuid"
                ") "
                "ON CONFLICT (user_id, key) DO UPDATE SET "
                "value = EXCLUDED.value, "
                "source_session_id = EXCLUDED.source_session_id, "
                "updated_at = now()",
                user_id,
                key,
                value_json,
                source_session_id,
            )

        await self._with_retry("upsert_pref", _insert)

    async def load_prefs(self, user_id: int, *, limit: int) -> list[Pref]:
        """Top-`limit` preferences by `updated_at DESC` (the injection read).

        Tiebreak: `key ASC`, so rows written in the same microsecond come
        back in a deterministic order. `limit <= 0` short-circuits to `[]`
        (a negative `LIMIT` is a SQL error; 0 is the documented "inject
        nothing" config value).

        `value::text` + `json.loads`: asyncpg hands jsonb back as a raw
        string by default, and callers want the decoded Python object.
        `source_session_id` is normalized to `str` (asyncpg decodes the
        uuid column to `uuid.UUID`) to honour the `Pref` annotation.
        """

        async def _select(conn: asyncpg.Connection) -> list[Pref]:
            rows = await conn.fetch(
                "SELECT key, value::text AS value, source_session_id, updated_at "
                "FROM chat_user_prefs "
                "WHERE user_id = $1 "
                "ORDER BY updated_at DESC, key ASC "
                "LIMIT $2",
                user_id,
                limit,
            )
            return [_row_to_pref(r) for r in rows]

        if limit <= 0:
            return []
        return await self._with_retry("load_prefs", _select)

    async def list_all_prefs_for_user(self, user_id: int) -> list[Pref]:
        """Export read: every preference row for the user, unpaginated.

        Sibling of `load_prefs` for the GDPR export
        (openspec/changes/agent-gdpr-erasure "prefs (every
        `agent.chat_user_prefs` row for the user)"): no LIMIT (export
        completeness) and `key ASC` ordering for a deterministic dump,
        rather than `load_prefs`'s injection-oriented `updated_at DESC`.
        Same `value::text` decode and `source_session_id` normalization.
        """

        async def _select(conn: asyncpg.Connection) -> list[Pref]:
            rows = await conn.fetch(
                "SELECT key, value::text AS value, source_session_id, updated_at "
                "FROM chat_user_prefs "
                "WHERE user_id = $1 "
                "ORDER BY key ASC",
                user_id,
            )
            return [_row_to_pref(r) for r in rows]

        return await self._with_retry("list_all_prefs_for_user", _select)

    async def delete_prefs_for_user(self, user_id: int) -> int:
        """Delete every preference row for one user. Returns the count.

        The user-scoped erasure hook (slice 4's GDPR path and the
        retention backstop for NULL-sourced rows, design.md D6).
        """

        async def _delete(conn: asyncpg.Connection) -> int:
            rows = await conn.fetch(
                "DELETE FROM chat_user_prefs WHERE user_id = $1 RETURNING id",
                user_id,
            )
            return len(rows)

        return await self._with_retry("delete_prefs_for_user", _delete)

    async def delete_prefs_for_sessions(self, session_ids: list[str]) -> int:
        """Delete prefs sourced from the given sessions. Returns the count.

        Retention coupling (design.md D6): called by the chat-retention
        cron in the same iteration as the session hard-delete, so derived
        memory never outlives the conversation it came from.

        Deletion keys on the exact ids the caller supplies. Safety is by
        construction: the retention caller hard-deletes the sessions
        first and passes the ids it just captured from its own
        `DELETE ... RETURNING id`, so only prefs derived from those
        sessions can match. (An earlier `USING chat_sessions` existence
        join was dropped: at this call site the session rows are already
        hard-deleted, so the join matched nothing and the cleanup was a
        silent no-op — and it added no owner filter, since a foreign
        live session id would have matched it equally.) Prefs sourced
        from ids NOT in the list — including NULL-sourced rows — are
        untouched; unrelated sessions' prefs survive.

        Empty `session_ids` is a no-op fast path: no query is issued.
        """
        if not session_ids:
            return 0

        async def _delete(conn: asyncpg.Connection) -> int:
            rows = await conn.fetch(
                "DELETE FROM chat_user_prefs "
                "WHERE source_session_id = ANY($1::uuid[]) "
                "RETURNING id",
                session_ids,
            )
            return len(rows)

        return await self._with_retry("delete_prefs_for_sessions", _delete)

    async def delete_orphaned(self) -> int:
        """Delete prefs whose source session no longer exists. Returns the count.

        Backstop for the retention coupling (review 2026-10-14 / H1):
        `delete_prefs_for_sessions` runs on a separate connection AFTER
        the session rows are gone, so a transient failure (or a crash
        between the two statements) would strand those prefs forever —
        the ids can never be matched again. The retention iteration
        calls this sweep every tick, so stranded rows are collected on
        the next pass. It also makes a metric possible
        (`agent_chat_prefs_deleted_total`), so the gap is observable
        rather than silent.

        `NOT EXISTS` is the right form HERE (unlike the hot-path inline
        delete — see `delete_prefs_for_sessions`): orphan detection is
        precisely the "session row already gone" case. NULL-sourced
        prefs are deliberately kept — they follow the slice-4
        user-scoped erasure path, not this one.
        """

        async def _delete(conn: asyncpg.Connection) -> int:
            rows = await conn.fetch(
                "DELETE FROM chat_user_prefs p "
                "WHERE p.source_session_id IS NOT NULL "
                "AND NOT EXISTS ("
                "  SELECT 1 FROM chat_sessions s WHERE s.id = p.source_session_id"
                ") "
                "RETURNING p.id",
            )
            return len(rows)

        return await self._with_retry("delete_orphaned", _delete)


# --- the gate heuristic (design.md D3) ---------------------------------------

# First-person preference markers, documented per pattern. The gate is
# deliberately conservative: only an explicit first-person subject fires,
# small talk ("今天天气怎么样", "thanks!") and second-person/request
# phrasing ("I would like you to summarize this") do not. False
# negatives are accepted (the user re-states or a later phrasing
# matches); false positives cost one small LLM call.
_PREFERENCE_PATTERNS: tuple[str, ...] = (
    # English positive markers: "I like / love / prefer / enjoy / adore X",
    # including the common intensifier inserts ("I really like",
    # "I kinda like").
    (
        r"\bi\s+(?:really\s+|quite\s+|kinda\s+|kind\s+of\s+)?"
        r"(?:like|love|prefer|enjoy|adore)\b"
    ),
    # "I'm into X" / "I am into X", with intensifiers ("I'm really into X").
    r"\bi(?:'m|\s+am)\s+(?:really\s+|very\s+|quite\s+)?into\b",
    # "I'm a (big) fan of X".
    r"\bi(?:'m|\s+am)\s+(?:a\s+)?(?:big\s+)?fan\s+of\b",
    # English negation / correction forms ("I no longer like X",
    # "I don't like X anymore", "I used to like X") — a changed mind must
    # reach extraction so the corrected value overwrites the stale one
    # (design.md risk "Injection leaks stale preferences").
    r"\bi\s+no\s+longer\s+(?:like|love|prefer|enjoy)\b",
    r"\bi\s+don'?t\s+(?:like|love|prefer|enjoy)\b.{0,40}?\banymore\b",
    r"\bi\s+used\s+to\s+(?:like|love|prefer|enjoy)\b",
    # Chinese positive markers ("我喜欢X" / "我爱X" / "我偏好X" / "我最爱X"),
    # with common intensifier inserts ("我很喜欢", "我超喜欢"). `爱` excludes
    # a following `人` so the set phrase "我爱人…" ("my spouse…") — a
    # third-person statement — does not fire the gate; a deliberate
    # false-negative trade under D3.
    r"我\s*(?:很|超|最|特别|非常|挺|好|蛮)?\s*(?:喜欢|爱(?!人)|偏好|最爱|中意)",
    # Identity-shaped fandom statements ("我是一个动漫迷", "我是…的粉丝").
    r"我是一个.{0,20}?迷",
    r"我是.{0,20}?的粉丝",
    # Chinese negation / correction forms ("我不再喜欢X了", "我以前喜欢X").
    r"我\s*(?:不再|已经不再|不)\s*喜欢",
    r"我(?:以前|曾经)喜欢",
)

_PREFERENCE_RE = re.compile("|".join(_PREFERENCE_PATTERNS), re.IGNORECASE)


def looks_like_preference(text: str) -> bool:
    """True when `text` carries a first-person preference signal.

    The gate that decides whether the (async) extraction LLM call fires
    (spec "Lightweight gate skips non-preference messages"). Patterns are
    documented at `_PREFERENCE_PATTERNS`; EN and ZH covered.
    """
    return _PREFERENCE_RE.search(text) is not None


# --- extraction (design.md D2/D4) --------------------------------------------

EXTRACTION_SYSTEM_PROMPT = """You extract durable user preferences from a single user message.

Return ONLY a JSON array (no prose, no markdown fences). Each element is:
{"key": "<short_snake_case_key>", "value": <JSON value>, "confidence": <0..1>}

Rules:
- Extract only preferences the user states about THEMSELVES, in the first person.
- `key`: a short snake_case identifier for the preference dimension ("snack", "music_genre", "editor").
- `value`: the stated preference as a JSON value (string, array, or object), not a sentence.
- Negations and corrections ("I no longer like X", "我不再喜欢X了", "我现在爱吃Y了") record the CURRENT state, never the old one — e.g. "我不再喜欢鱿鱼丝了，现在爱吃猪肉脯" yields {"key": "snack", "value": "猪肉脯"}. If the message only says the user stopped liking something and states no replacement, return [] — there is no current value to record.
- Return [] when the message states no preference, is a question, or is small talk.
- `confidence`: 0..1, how certain the statement is.
"""


class _ExtractedPref(BaseModel):
    """One element of the LLM's JSON array.

    `key` / `value` are required (a row without a key has nothing to
    upsert into); `confidence` defaults so a reply that omits it still
    validates.
    """

    key: str
    value: Any
    confidence: float = 1.0


# --- extraction bounds (review 2026-10-14 / M1) -------------------------------
# LLM output is untrusted and lands in the planner SYSTEM prompt. These
# caps bound both the persisted row and the rendered block. The key
# charset deliberately excludes whitespace/newlines: a key like
# "snack\nSYSTEM: ignore..." must never be able to inject prompt lines.
_KEY_RE = re.compile(r"[a-z0-9_]{1,64}")
_VALUE_MAX_CHARS = 512
_MAX_PREFS_PER_TURN = 20
# Mirror the planner's client bounds (planner.py) so a runaway
# extraction cannot stream for the chat request's whole lifetime.
_EXTRACTION_MAX_TOKENS = 512
_EXTRACTION_TIMEOUT_SECONDS = 30


# ```json ... ``` / ``` ... ``` wrapper — models routinely fence JSON output.
_FENCE_RE = re.compile(r"^\s*```(?:json)?\s*(.*?)\s*```\s*$", re.DOTALL | re.IGNORECASE)


def _strip_code_fence(text: str) -> str:
    """Drop a fenced code block wrapper, if present. Non-fenced input passes through."""
    match = _FENCE_RE.match(text)
    return match.group(1) if match else text.strip()


def _content_text(content: Any) -> str:
    """Flatten an AIMessage `content` to text.

    Normally a plain string; some providers / SDK versions emit a list of
    content blocks, so text blocks are joined. Anything else is
    stringified so the parse stage fails loudly (WARN) instead of
    crashing.
    """
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for block in content:
            if isinstance(block, str):
                parts.append(block)
            elif isinstance(block, dict) and block.get("type") == "text":
                parts.append(str(block.get("text", "")))
        return "".join(parts)
    return str(content)


def _parse_extraction(raw: str) -> list[Pref]:
    """Parse the LLM's reply into `Pref` rows.

    Raises on malformed output (not JSON / not an array / an element
    failing `_ExtractedPref`) — the caller logs and returns `[]`.

    LLM output is untrusted input and lands in the planner SYSTEM
    prompt (review 2026-10-14 / M1), so every field is bounded here:

      - key: normalized to lower case + must match `_KEY_RE`
        (`^[a-z0-9_]{1,64}$`); anything else is dropped. This is what
        keeps a hostile or sloppy extraction from writing newlines /
        instructions into the persisted prompt block.
      - value: serialized with `json.dumps`; entries whose serialized
        form exceeds `_VALUE_MAX_CHARS` (512) are dropped (a preference
        that large is not a preference).
      - at most `_MAX_PREFS_PER_TURN` (20) entries per reply are kept —
        bounds both the upsert round-trips and prompt growth.
    """
    payload = _json.loads(_strip_code_fence(raw))
    if not isinstance(payload, list):
        # ValueError, not TypeError: this is a malformed untrusted payload
        # (caught and warn-logged by `extract_preferences`), not a bad
        # argument type from a caller.
        raise ValueError(  # noqa: TRY004
            f"extraction reply is not a JSON array (got {type(payload).__name__})"
        )
    prefs: list[Pref] = []
    for item in payload:
        validated = _ExtractedPref.model_validate(item)
        key = validated.key.strip().lower()
        if not _KEY_RE.fullmatch(key):
            log.warning("extraction: dropping pref with invalid key %r", key)
            continue
        value_json = _json.dumps(validated.value, ensure_ascii=False, sort_keys=True)
        if len(value_json) > _VALUE_MAX_CHARS:
            log.warning(
                "extraction: dropping pref %r with oversized value (%d chars)",
                key,
                len(value_json),
            )
            continue
        prefs.append(
            Pref(key=key, value=validated.value, confidence=validated.confidence)
        )
        if len(prefs) >= _MAX_PREFS_PER_TURN:
            log.warning(
                "extraction: capped at %d prefs for this turn", _MAX_PREFS_PER_TURN
            )
            break
    return prefs


async def extract_preferences(
    user_message: str, *, settings: Settings
) -> list[Pref]:
    """One small LLM call → validated preferences. NEVER raises.

    The caller runs this as a fire-and-forget task after the reply is
    recorded (design.md D2), so every failure mode — client
    construction, LLM error, malformed output — logs a WARNING and
    returns `[]`; letting an exception escape would only surface as an
    unretrieved-exception log. Not retried within the run, and NOT
    charged to the per-user chat budget (design.md D4 — internal
    operation).

    `ChatOpenAI` is imported at module level and constructed per call
    (cheap, mirrors `planner._build_structured_client`), so tests patch
    `prefs.ChatOpenAI`, the same pattern as `planner.ChatOpenAI`. The
    `or "no-key"` api-key fallback matches planner.py: ChatOpenAI raises
    at construction on an empty key, and local vLLM needs none.
    `max_tokens` + `timeout` mirror the planner's bounds so a runaway
    extraction cannot stream for the chat request's whole lifetime.
    """
    try:
        client = ChatOpenAI(
            model=settings.planner_model,
            base_url=settings.llm_base_url,
            api_key=SecretStr(settings.llm_api_key or "no-key"),
            temperature=0,
            max_tokens=_EXTRACTION_MAX_TOKENS,
            timeout=_EXTRACTION_TIMEOUT_SECONDS,
        )
        response = await client.ainvoke(
            [
                SystemMessage(content=EXTRACTION_SYSTEM_PROMPT),
                HumanMessage(content=user_message),
            ]
        )
        return _parse_extraction(_content_text(response.content))
    except Exception as exc:  # noqa: BLE001 — fire-and-forget contract: never raise
        log.warning("prefs extraction failed, returning [] (%r)", exc)
        return []


# --- planner injection (design.md D5) ----------------------------------------

_PREFS_HEADER = "Known user preferences:"


def prefs_block(prefs: list[Pref]) -> str:
    """Render the planner prompt block for `prefs`.

    Format (documented contract): `""` for an empty list — byte parity
    with the pre-injection prompt, so callers append unconditionally
    (`PLANNER_SYSTEM_PROMPT + planner_block(settings) + prefs_block(prefs)`).
    Otherwise a "\\n\\n" separator (mirrors `planner_block`), one header
    line `Known user preferences:`, then one `- {key}: {json value}` line
    per preference sorted by key (deterministic across calls; the stored
    order is not load-order dependent), no trailing whitespace. Values
    render as compact JSON (`json.dumps`, non-ASCII kept verbatim).
    """
    if not prefs:
        return ""
    lines = [
        # Defence in depth (review M1): extraction already restricts keys
        # to `_KEY_RE`, but the block must not be able to carry injected
        # prompt lines even if a row predates that validation or was
        # written by hand — newlines in a key are flattened here.
        f"- {p.key.replace(chr(10), ' ').replace(chr(13), ' ')}: "
        f"{_json.dumps(p.value, ensure_ascii=False, sort_keys=True)}"
        for p in sorted(prefs, key=lambda p: p.key)
    ]
    return "\n\n" + "\n".join([_PREFS_HEADER, *lines])


def _row_to_pref(row: asyncpg.Record | Any) -> Pref:
    raw_value = row["value"]
    raw_session_id = row["source_session_id"]
    return Pref(
        key=row["key"],
        value=_json.loads(raw_value),
        source_session_id=(
            str(raw_session_id) if raw_session_id is not None else None
        ),
        updated_at=row["updated_at"],
    )
