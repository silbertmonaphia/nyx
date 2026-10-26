"""Shared guards for the agent_py test suite.

Most DB-backed tests here DROP and recreate the `agent.*` and
`langgraph.*` schemas against whatever `TEST_ASYNC_PG_DSN` names. Against
the shared dev database (`nyx`) that is destructive to live data — an
operator's running stack, a parallel session's e2e, or chat history
being actively written all share that DB (observed live, 2026-10-14: a
reviewer's pytest run dropped the schemas mid-e2e).

Point the suite at a disposable database instead. CI uses `nyx_test`
(see `.github/workflows/ci.yml`); the local dev DB has `nyx_test`
pre-created for the same purpose.
"""
from __future__ import annotations

import os
from urllib.parse import urlparse

import pytest

from tests.service_auth_fixtures import TEST_SERVICE_TOKEN

os.environ["AGENT_SERVICE_TOKEN"] = TEST_SERVICE_TOKEN

_DSN = os.environ.get("TEST_ASYNC_PG_DSN")


def pytest_configure(config: pytest.Config) -> None:
    if not _DSN:
        # Not set → every DB-backed test self-skips; nothing to guard.
        return
    db_name = urlparse(_DSN).path.lstrip("/")
    if db_name != "nyx_test":
        raise pytest.UsageError(
            f"TEST_ASYNC_PG_DSN names database {db_name!r}, but this suite's "
            "fixtures DROP and recreate the agent.* / langgraph.* schemas. "
            "That is only safe against a disposable database. Point the DSN "
            "at `nyx_test` (CI's convention; pre-created locally) — never "
            "the shared dev `nyx` DB."
        )
