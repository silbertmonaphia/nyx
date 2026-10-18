"""Fold the run's SSE step frames into the persisted assistant step trace.

The live panel renders a run from the frames it receives; replay rebuilds
steps from this trace instead, so both views of one turn agree. The fold
is pure and frame-fed (no graph state, no I/O): `main.py`'s drain forwards
every frame exactly as it reached the client, and the finalize write
persists `to_list()` on the assistant row.

Frames and their effect on one entry per step id:

  - `plan`           seed every planned step in plan order (`kind`,
                     `tool_name` from `tool`, `tool_args` from `args`,
                     status `running`) without downgrading a terminal
                     status a re-emitted frame already produced.
  - `step_start`     upsert; latest `kind`; status `running` unless the
                     step already reached `ok` / `failed`.
  - `tool_call`      upsert; latest `tool_name` / `tool_args`.
  - `tool_result`    upsert; `tool_result` is `result` on success else
                     `error`; `retry_count` (0 when the wire omits it).
  - `confirm_request` upsert; `tool_name` / `tool_args` only — approval
                     cards are deliberately not persisted.
  - `step_end`       upsert; terminal `status` (`ok` / `failed`) and
                     `reason` when supplied.

Every other frame kind is ignored. Folding is idempotent: a resumed run
that replays already-emitted frames lands on the same entries with the
latest value winning per field. `to_list()` returns `None` for an empty
trace so the column stays NULL for runs that planned no steps.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

_TERMINAL_STATUSES = frozenset({"ok", "failed"})

_OPTIONAL_FIELDS = (
    "tool_name",
    "tool_args",
    "tool_result",
    "retry_count",
    "reason",
)


class StepTrace:
    """Accumulate `(kind, payload)` frame pairs into ordered step entries."""

    def __init__(self) -> None:
        self._entries: dict[str, dict[str, Any]] = {}

    def fold(self, kind: str, payload: Mapping[str, Any] | None) -> None:
        if not isinstance(payload, Mapping):
            return
        if kind == "plan":
            self._fold_plan(payload)
        elif kind == "step_start":
            self._fold_step_start(payload)
        elif kind == "tool_call":
            self._fold_tool_call(payload)
        elif kind == "tool_result":
            self._fold_tool_result(payload)
        elif kind == "confirm_request":
            self._fold_confirm_request(payload)
        elif kind == "step_end":
            self._fold_step_end(payload)

    def to_list(self) -> list[dict[str, Any]] | None:
        if not self._entries:
            return None
        entries: list[dict[str, Any]] = []
        for entry in self._entries.values():
            row: dict[str, Any] = {"id": entry["id"]}
            row["kind"] = entry.get("kind") or "tool"
            row["status"] = entry["status"]
            for field in _OPTIONAL_FIELDS:
                if entry.get(field) is not None:
                    row[field] = entry[field]
            entries.append(row)
        return entries

    def _fold_plan(self, payload: Mapping[str, Any]) -> None:
        steps = payload.get("steps")
        if not isinstance(steps, list):
            return
        for raw_step in steps:
            if not isinstance(raw_step, Mapping):
                continue
            step_id = raw_step.get("id")
            if not _valid_step_id(step_id):
                continue
            entry = self._ensure(step_id)
            kind = raw_step.get("kind")
            if kind is not None:
                entry["kind"] = _kind_value(kind)
            self._running_unless_terminal(entry)
            tool = raw_step.get("tool")
            if tool is not None:
                entry["tool_name"] = tool
            args = raw_step.get("args")
            if args is not None:
                entry["tool_args"] = args

    def _fold_step_start(self, payload: Mapping[str, Any]) -> None:
        entry = self._entry_for(payload)
        if entry is None:
            return
        kind = payload.get("kind")
        if kind is not None:
            entry["kind"] = _kind_value(kind)
        self._running_unless_terminal(entry)

    def _fold_tool_call(self, payload: Mapping[str, Any]) -> None:
        entry = self._entry_for(payload)
        if entry is None:
            return
        name = payload.get("name")
        if name is not None:
            entry["tool_name"] = name
        args = payload.get("args")
        if args is not None:
            entry["tool_args"] = args

    def _fold_tool_result(self, payload: Mapping[str, Any]) -> None:
        entry = self._entry_for(payload)
        if entry is None:
            return
        if payload.get("ok") is True:
            entry["tool_result"] = payload.get("result")
        else:
            entry["tool_result"] = payload.get("error")
        entry["retry_count"] = _retry_count(payload.get("retry_count"))

    def _fold_confirm_request(self, payload: Mapping[str, Any]) -> None:
        entry = self._entry_for(payload)
        if entry is None:
            return
        tool = payload.get("tool")
        if tool is not None:
            entry["tool_name"] = tool
        args = payload.get("args")
        if args is not None:
            entry["tool_args"] = args

    def _fold_step_end(self, payload: Mapping[str, Any]) -> None:
        entry = self._entry_for(payload)
        if entry is None:
            return
        status = payload.get("status")
        if status not in _TERMINAL_STATUSES:
            return
        entry["status"] = status
        reason = payload.get("reason")
        if reason is None:
            entry.pop("reason", None)
        else:
            entry["reason"] = reason

    def _entry_for(self, payload: Mapping[str, Any]) -> dict[str, Any] | None:
        step_id = payload.get("step_id")
        if not _valid_step_id(step_id):
            return None
        return self._ensure(step_id)

    def _ensure(self, step_id: str) -> dict[str, Any]:
        entry = self._entries.get(step_id)
        if entry is None:
            entry = {"id": step_id, "status": "running"}
            self._entries[step_id] = entry
        return entry

    @staticmethod
    def _running_unless_terminal(entry: dict[str, Any]) -> None:
        if entry.get("status") not in _TERMINAL_STATUSES:
            entry["status"] = "running"


def _valid_step_id(value: Any) -> bool:
    return isinstance(value, str) and bool(value)


def _kind_value(raw: Any) -> Any:
    return getattr(raw, "value", raw)


def _retry_count(raw: Any) -> int:
    if isinstance(raw, bool) or not isinstance(raw, int):
        return 0
    return raw
