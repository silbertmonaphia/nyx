"""Reference grammar for tool-step arguments.

A reference is the entire string value ``{{step:<step_id>}}`` and resolves
to the output of an earlier step in the same plan. The executor performs
the substitution at execution time; the planner only has to author the
reference. Embedded interpolation (``prefix {{step:id}}``) is malformed:
the reference must be the whole value, so a partially templated string
fails closed instead of reaching a tool.

The module is dependency-free on purpose: ``planner.py`` imports it for
the compose-before-write guard without pulling in executor machinery.
"""

from __future__ import annotations

import re
from collections.abc import Iterator
from dataclasses import dataclass
from typing import Any

REFERENCE_RE = re.compile(r"^\{\{step:(?P<step_id>[^{}\s]+)\}\}\Z")
_MARKER_RE = re.compile(r"\{\{|\}\}")

PathPart = str | int


@dataclass(frozen=True)
class ArgReference:
    step_id: str
    path: tuple[PathPart, ...]


@dataclass(frozen=True)
class MalformedReference:
    path: tuple[PathPart, ...]
    text: str


@dataclass(frozen=True)
class ArgScan:
    references: tuple[ArgReference, ...]
    malformed: tuple[MalformedReference, ...]

    @property
    def has_references(self) -> bool:
        return bool(self.references or self.malformed)


def _walk(
    value: Any, path: tuple[PathPart, ...]
) -> Iterator[ArgReference | MalformedReference]:
    if isinstance(value, str):
        match = REFERENCE_RE.match(value)
        if match is not None:
            yield ArgReference(step_id=match.group("step_id"), path=path)
        elif _MARKER_RE.search(value):
            yield MalformedReference(path=path, text=value)
        return
    if isinstance(value, dict):
        for key, child in value.items():
            yield from _walk(child, path + (key,))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            yield from _walk(child, path + (index,))


def iter_references(args: Any) -> Iterator[ArgReference | MalformedReference]:
    """Yield every reference or malformed marker in a JSON-shaped args value."""
    yield from _walk(args, ())


def scan_args(args: Any) -> ArgScan:
    """Split an args value into whole-value references and malformed markers."""
    references: list[ArgReference] = []
    malformed: list[MalformedReference] = []
    for item in iter_references(args):
        if isinstance(item, ArgReference):
            references.append(item)
        else:
            malformed.append(item)
    return ArgScan(tuple(references), tuple(malformed))


def has_reference_to(args: Any, step_id: str) -> bool:
    """True when any string leaf is exactly a reference to ``step_id``."""
    return any(
        isinstance(item, ArgReference) and item.step_id == step_id
        for item in iter_references(args)
    )
