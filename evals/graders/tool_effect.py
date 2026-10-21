"""Execution-effect grader for state-changing cases.

`expect.tool_effect` pins the write the case exists to grade. Plan shape alone
is demonstrably insufficient: the recorded pre-edit baseline names
`update_feed`/`delete_feed` in the plan, the executed call fails schema
validation, the retry substitutes `list_feeds`, and a shape-only grader still
scores the sample green.

The contract:

- `tool` is a destructive tool (`create_feed`, `update_feed`, `delete_feed`),
  `effect` is `created` / `updated` / `deleted`, `step_index` optionally pins
  the plan step, `description_any_of` is required for `created`,
  `success_claims` lists phrases the answer may only utter after a write, and
  `min_effects` optionally requires that many distinct successful targets so a
  single-target write cannot satisfy a plural request.
- The expected step is the last turn's last `plan` frame's step naming the
  tool. Without `step_index`, exactly one such step must exist.
- Every `tool_call` under that step must be for the expected tool, carry a
  `call_id`, and have a correlated `tool_result`; at least one result must be
  `ok: true`. A retry that substitutes a different tool, a missing result, or
  a missing `call_id` fails the sample even when plan tokens match.
- The feed effect is read from `result.structuredContent`, then a JSON
  `result.content[].text`, then the provider's `meta.feeds_before` /
  `meta.feeds_after` snapshot. A created feed's description must contain one
  of the declared keywords, so a plan-time placeholder cannot pass.
- Declared `success_claims` are forbidden when no successful write was
  recorded and permitted once it was.

No LLM judge: every check is a deterministic frame/payload assertion.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from _common import (  # noqa: E402
    DESTRUCTIVE_TOOLS,
    clip,
    expect_of,
    frame_data,
    frames_of,
    grader,
    last_plan_steps,
    turn_answer,
    turns_of,
    verdict,
)

EXPECTED_EFFECTS = {
    "create_feed": "created",
    "update_feed": "updated",
    "delete_feed": "deleted",
}

SPEC_KEYS = frozenset(
    {
        "tool",
        "effect",
        "step_index",
        "description_any_of",
        "success_claims",
        "min_effects",
    }
)


def _validate_spec(spec) -> str | None:
    if not isinstance(spec, dict):
        return "expect.tool_effect is not a mapping; nothing to grade"
    unknown = sorted(set(spec) - SPEC_KEYS)
    if unknown:
        return f"tool_effect has unknown key(s) {unknown}"
    tool = spec.get("tool")
    if not isinstance(tool, str) or tool not in DESTRUCTIVE_TOOLS:
        return f"tool_effect.tool {tool!r} is not one of {sorted(DESTRUCTIVE_TOOLS)}"
    expected = EXPECTED_EFFECTS.get(tool)
    effect = spec.get("effect")
    if effect != expected:
        return (
            f"tool_effect.effect {effect!r} does not match tool {tool!r} "
            f"(expected {expected!r})"
        )
    step_index = spec.get("step_index")
    if step_index is not None and (
        isinstance(step_index, bool)
        or not isinstance(step_index, int)
        or step_index < 0
    ):
        return f"tool_effect.step_index {step_index!r} is not a non-negative integer"
    min_effects = spec.get("min_effects")
    if min_effects is not None and (
        isinstance(min_effects, bool)
        or not isinstance(min_effects, int)
        or min_effects < 1
    ):
        return "tool_effect.min_effects must be a positive integer"
    keywords = spec.get("description_any_of")
    if keywords is not None and (
        not isinstance(keywords, list)
        or not keywords
        or not all(isinstance(item, str) and item for item in keywords)
    ):
        return "tool_effect.description_any_of must be a non-empty list of strings"
    if effect == "deleted" and keywords is not None:
        return "tool_effect.description_any_of does not apply to a delete effect"
    claims = spec.get("success_claims")
    if claims is not None and (
        not isinstance(claims, list)
        or not all(isinstance(item, str) and item for item in claims)
    ):
        return "tool_effect.success_claims must be a list of strings"
    if effect == "created" and keywords is None:
        return "tool_effect.description_any_of is required for a created effect"
    return None


def _describe_step(step: dict) -> str:
    if step.get("kind") == "tool":
        return f"tool {step.get('tool')!r}"
    return f"kind {step.get('kind')!r}"


def _select_step(
    steps, tool: str, step_index: int | None
) -> tuple[dict | None, int | None, str | None]:
    if not isinstance(steps, list) or not steps:
        return None, None, "the last plan frame carries no steps"
    if step_index is not None:
        if step_index >= len(steps):
            return (
                None,
                None,
                f"plan has {len(steps)} step(s); step_index {step_index} is out of range",
            )
        step = steps[step_index]
        if not isinstance(step, dict):
            return None, None, f"plan step {step_index} is malformed"
        if step.get("kind") != "tool" or step.get("tool") != tool:
            return (
                None,
                None,
                f"plan step {step_index} is {_describe_step(step)}, not tool {tool!r}",
            )
        return step, step_index, None
    matches = [
        (index, step)
        for index, step in enumerate(steps)
        if isinstance(step, dict)
        and step.get("kind") == "tool"
        and step.get("tool") == tool
    ]
    if not matches:
        return None, None, f"the plan names no {tool!r} step"
    if len(matches) > 1:
        return (
            None,
            None,
            f"the plan names {tool!r} in {len(matches)} steps; pin tool_effect.step_index",
        )
    index, step = matches[0]
    return step, index, None


def _step_frames(turn: dict, step_id: str) -> list[dict]:
    return [
        frame
        for frame in frames_of(turn)
        if frame_data(frame).get("step_id") == step_id
    ]


def _executed_tools(frames: list[dict]) -> list[str]:
    names: list[str] = []
    for frame in frames:
        if frame.get("event") != "tool_call":
            continue
        name = frame_data(frame).get("name")
        names.append(name if isinstance(name, str) else "<missing-name>")
    return names


def _collect_calls(
    frames: list[dict], tool: str
) -> tuple[list[dict], list[str]]:
    calls: list[dict] = []
    others: list[str] = []
    for frame in frames:
        if frame.get("event") != "tool_call":
            continue
        data = frame_data(frame)
        name = data.get("name")
        if name == tool:
            calls.append(data)
        else:
            others.append(name if isinstance(name, str) else "<missing-name>")
    return calls, others


def _results_by_call_id(frames: list[dict]) -> dict[str, list[dict]]:
    mapping: dict[str, list[dict]] = {}
    for frame in frames:
        if frame.get("event") != "tool_result":
            continue
        data = frame_data(frame)
        call_id = data.get("call_id")
        if isinstance(call_id, str) and call_id:
            mapping.setdefault(call_id, []).append(data)
    return mapping


def _evaluate_calls(
    calls: list[dict], results: dict[str, list[dict]]
) -> tuple[list[str], list[dict], list[str]]:
    failures: list[str] = []
    successful: list[dict] = []
    call_ids: list[str] = []
    for data in calls:
        call_id = data.get("call_id")
        if not isinstance(call_id, str) or not call_id:
            failures.append("a tool_call for the expected tool carries no call_id")
            continue
        call_ids.append(call_id)
        matches = results.get(call_id) or []
        if not matches:
            failures.append(f"tool_call {call_id} has no correlated tool_result")
            continue
        winners = [result for result in matches if result.get("ok") is True]
        if winners:
            successful.append(winners[0])
    if calls and not successful:
        errors = sorted(
            {
                str(result.get("error"))
                for result in results.values()
                for result in result
                if result.get("error")
            }
        )
        detail = f" ({'; '.join(errors)})" if errors else ""
        failures.append(f"no {calls[0].get('name')!r} tool_result with ok is true{detail}")
    return failures, successful, call_ids


def _content_payload(result: dict) -> tuple[dict | None, str | None]:
    """The MCP result payload: `result.result`, or the frame data when flat."""
    if not isinstance(result, dict):
        return None, None
    payload = result.get("result")
    if not isinstance(payload, dict):
        payload = result
    structured = payload.get("structuredContent")
    if isinstance(structured, dict) and structured:
        return structured, "structuredContent"
    content = payload.get("content")
    if isinstance(content, list):
        for entry in content:
            if not isinstance(entry, dict):
                continue
            text = entry.get("text")
            if not isinstance(text, str):
                continue
            try:
                decoded = json.loads(text)
            except ValueError:
                continue
            if isinstance(decoded, dict) and decoded:
                return decoded, "content_text"
    if isinstance(structured, list) and structured:
        return {"data": structured}, "structuredContent"
    return None, None


def _effect_target_count(results: list[dict]) -> int:
    targets: set[str] = set()
    metadata_seen = False
    for result in results:
        payload, _ = _content_payload(result)
        if not isinstance(payload, dict):
            continue
        value = payload.get("id")
        if value is not None:
            metadata_seen = True
            if isinstance(value, (int, str)) and not isinstance(value, bool):
                targets.add(str(value))
        for key in ("deleted_ids", "updated_ids", "ids", "data"):
            items = payload.get(key)
            if not isinstance(items, list):
                continue
            metadata_seen = True
            for item in items:
                item_id = item.get("id") if isinstance(item, dict) else item
                if isinstance(item_id, (int, str)) and not isinstance(item_id, bool):
                    targets.add(str(item_id))
    if targets:
        return len(targets)
    return 0 if metadata_seen else len(results)


def _feed_row(payload) -> dict | None:
    if not isinstance(payload, dict):
        return None
    if isinstance(payload.get("description"), str) or isinstance(
        payload.get("title"), str
    ):
        return payload
    nested = payload.get("feed")
    if isinstance(nested, dict):
        return _feed_row(nested)
    return None


def _snapshot(transcript: dict, key: str) -> list[dict] | None:
    meta = transcript.get("meta")
    if not isinstance(meta, dict):
        return None
    rows = meta.get(key)
    if not isinstance(rows, list):
        return None
    return [row for row in rows if isinstance(row, dict)]


def _snapshot_error(transcript: dict) -> str | None:
    meta = transcript.get("meta")
    if not isinstance(meta, dict):
        return None
    error = meta.get("feeds_snapshot_error")
    return error if isinstance(error, str) and error else None


def _feed_key(row: dict) -> tuple[str, str]:
    if isinstance(row.get("id"), (int, str)) and not isinstance(row.get("id"), bool):
        return ("id", str(row["id"]))
    return ("title", str(row.get("title")))


def _new_feeds(before: list[dict], after: list[dict]) -> list[dict]:
    known = {_feed_key(row) for row in before}
    return [row for row in after if _feed_key(row) not in known]


def _changed_feeds(before: list[dict], after: list[dict]) -> list[dict]:
    old_by_key = {_feed_key(row): row for row in before}
    changed: list[dict] = []
    for row in after:
        old = old_by_key.get(_feed_key(row))
        if old is None:
            continue
        if (
            old.get("title") != row.get("title")
            or old.get("description") != row.get("description")
            or old.get("updated_at") != row.get("updated_at")
        ):
            changed.append(row)
    return changed


def _description_matches(row: dict, keywords: list[str]) -> bool:
    description = row.get("description")
    return isinstance(description, str) and any(
        keyword in description for keyword in keywords
    )


def _verify_created(
    transcript: dict, spec: dict, payload: dict | None, source: str | None
) -> tuple[dict | None, str | None, str | None]:
    keywords = spec.get("description_any_of") or []
    row = _feed_row(payload) if payload is not None else None
    used_source = source
    if row is None:
        before = _snapshot(transcript, "feeds_before")
        after = _snapshot(transcript, "feeds_after")
        if before is not None and after is not None:
            candidates = _new_feeds(before, after)
            if len(candidates) == 1:
                row = candidates[0]
                used_source = "snapshot"
            elif len(candidates) > 1:
                matching = [
                    candidate
                    for candidate in candidates
                    if _description_matches(candidate, keywords)
                ]
                if len(matching) == 1:
                    row = matching[0]
                    used_source = "snapshot"
                else:
                    return (
                        None,
                        used_source,
                        f"the snapshot shows {len(candidates)} new feeds; "
                        "cannot identify the created one",
                    )
        if row is None:
            snapshot_error = _snapshot_error(transcript)
            if snapshot_error:
                return (
                    None,
                    used_source,
                    "no created feed in the tool_result payload and the feed "
                    f"snapshot failed ({snapshot_error})",
                )
            return (
                None,
                used_source,
                "no created feed in the tool_result payload or the feed snapshot",
            )
    description = row.get("description")
    if not isinstance(description, str):
        return row, used_source, "the created feed carries no description"
    if not any(keyword in description for keyword in keywords):
        return (
            row,
            used_source,
            f"the created feed description {clip(description)!r} contains none "
            f"of {keywords!r}",
        )
    return row, used_source, None


def _verify_updated(
    transcript: dict, spec: dict, payload: dict | None, source: str | None
) -> tuple[dict | None, str | None, str | None]:
    keywords = spec.get("description_any_of") or []
    row = _feed_row(payload) if payload is not None else None
    if row is not None:
        if keywords and not _description_matches(row, keywords):
            return (
                row,
                source,
                f"the updated feed description {clip(str(row.get('description')))!r} "
                f"contains none of {keywords!r}",
            )
        return row, source, None
    before = _snapshot(transcript, "feeds_before")
    after = _snapshot(transcript, "feeds_after")
    if before is not None and after is not None:
        changed = _changed_feeds(before, after)
        if keywords:
            changed = [row for row in changed if _description_matches(row, keywords)]
        if changed:
            return changed[0], "snapshot", None
        return None, "snapshot", "the feed snapshot shows no changed feed"
    snapshot_error = _snapshot_error(transcript)
    if snapshot_error:
        return (
            None,
            None,
            "no updated feed in the tool_result payload and the feed snapshot "
            f"failed ({snapshot_error})",
        )
    return None, None, "no updated feed in the tool_result payload or the feed snapshot"


def _target_feed_id(payload: dict | None, calls: list[dict]) -> str | None:
    if isinstance(payload, dict) and payload.get("id") is not None:
        return str(payload["id"])
    for data in calls:
        args = data.get("args")
        if not isinstance(args, dict):
            continue
        for key in ("id", "feed_id"):
            if args.get(key) is not None:
                return str(args[key])
    return None


def _verify_deleted(
    transcript: dict, payload: dict | None, calls: list[dict], source: str | None
) -> tuple[dict | None, str | None, str | None]:
    if isinstance(payload, dict) and payload.get("deleted") is False:
        return (
            None,
            source,
            f"the delete tool_result reports deleted={payload.get('deleted')!r}",
        )
    target_id = _target_feed_id(payload, calls)
    before = _snapshot(transcript, "feeds_before")
    after = _snapshot(transcript, "feeds_after")
    if before is not None and after is not None:
        if target_id is not None:
            present = {str(row.get("id")) for row in before if row.get("id") is not None}
            remaining = {
                str(row.get("id")) for row in after if row.get("id") is not None
            }
            if target_id not in present:
                return (
                    None,
                    "snapshot",
                    f"feed id {target_id!r} was not present before the delete; "
                    "the call targeted a non-existent or placeholder id",
                )
            if target_id in remaining:
                return (
                    None,
                    "snapshot",
                    f"feed id {target_id!r} is still present after the delete",
                )
            removed = next(
                (row for row in before if str(row.get("id")) == target_id), None
            )
            return removed or {"id": target_id}, "snapshot", None
        if len(after) < len(before):
            return None, "snapshot", None
        if isinstance(payload, dict) and payload.get("deleted") is True:
            return payload, source, None
        return None, "snapshot", "the feed snapshot shows no removed feed"
    if isinstance(payload, dict) and payload.get("deleted") is True:
        return payload, source, None
    snapshot_error = _snapshot_error(transcript)
    if snapshot_error:
        return (
            None,
            None,
            "the delete result carries no deleted marker and the feed snapshot "
            f"failed ({snapshot_error})",
        )
    return (
        None,
        None,
        "the delete result carries no deleted marker and no feed snapshot is available",
    )


def _feed_metadata(row: dict | None) -> dict | None:
    if row is None:
        return None
    return {
        "id": row.get("id"),
        "title": row.get("title"),
        "description": clip(str(row.get("description"))),
    }


@grader
def get_assert(transcript: dict, context: dict) -> dict:
    expect = expect_of(context)
    spec = expect.get("tool_effect")
    error = _validate_spec(spec)
    if error:
        return verdict(False, error)

    tool = spec["tool"]
    effect = spec["effect"]
    turn_index, steps = last_plan_steps(transcript)
    if steps is None:
        return verdict(
            False,
            f"no plan frame in the last turn "
            f"(turn {turn_index} of {len(turns_of(transcript))})",
        )
    step, step_index, error = _select_step(steps, tool, spec.get("step_index"))
    metadata: dict = {
        "tool": tool,
        "effect": effect,
        "step_index": step_index,
        "plan_step": step,
    }
    if error:
        return verdict(False, error, metadata=metadata)
    step_id = step.get("id")
    if not isinstance(step_id, str) or not step_id:
        return verdict(
            False,
            f"plan step {step_index} has no id; tool frames cannot be correlated",
            metadata=metadata,
        )

    turns = turns_of(transcript)
    turn = turns[-1] if turns else {}
    frames = _step_frames(turn, step_id)
    calls, others = _collect_calls(frames, tool)
    result_failures, successful, call_ids = _evaluate_calls(
        calls, _results_by_call_id(frames)
    )
    failures: list[str] = []
    if others:
        failures.append(
            f"the expected step dispatched other tool(s) {sorted(set(others))}; "
            "a retry that substitutes a different tool fails"
        )
    if not calls:
        failures.append(f"no tool_call for {tool!r} under step {step_id!r}")
    failures.extend(result_failures)

    min_effects = spec.get("min_effects")
    observed_effects = _effect_target_count(successful)
    if min_effects is not None and observed_effects < min_effects:
        failures.append(
            f"expected at least {min_effects} {effect} effect(s), "
            f"observed {observed_effects} distinct target(s)"
        )

    answer = turn_answer(turn)
    claims = spec.get("success_claims") or []
    hits = [claim for claim in claims if claim in answer]
    metadata.update(
        {
            "step_id": step_id,
            "executed_tools": _executed_tools(frames),
            "call_ids": call_ids,
            "write_executed": bool(successful),
            "min_effects": min_effects,
            "observed_effects": observed_effects,
            "success_claims": claims,
            "answer_claim_hits": hits,
        }
    )

    if failures:
        if hits and not successful:
            failures.append(
                f"the answer claims success ({hits!r}) but no successful "
                f"{tool} write was recorded"
            )
        return verdict(
            False,
            f"execution effect failed for {tool} ({effect}): " + " | ".join(failures),
            metadata=metadata,
        )

    payload, source = _content_payload(successful[0])
    metadata["content_source"] = source
    if effect == "created":
        row, source, error = _verify_created(transcript, spec, payload, source)
    elif effect == "updated":
        row, source, error = _verify_updated(transcript, spec, payload, source)
    else:
        successful_ids = {
            result.get("call_id")
            for result in successful
            if isinstance(result.get("call_id"), str)
        }
        successful_calls = [
            call for call in calls if call.get("call_id") in successful_ids
        ]
        row, source, error = _verify_deleted(
            transcript, payload, successful_calls, source
        )
    metadata["content_source"] = source
    metadata["feed"] = _feed_metadata(row)
    if error:
        return verdict(
            False,
            f"execution effect failed for {tool} ({effect}): {error}",
            metadata=metadata,
        )
    return verdict(
        True,
        f"write effect ok: {tool} executed under step {step_id!r} "
        f"(effect={effect}, content_source={source or 'none'})",
        metadata=metadata,
    )
