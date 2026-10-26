"""Shared service-token constants for the agent test suite."""

from __future__ import annotations

TEST_SERVICE_TOKEN = "test-agent-service-token-0123456789abcdef"  # noqa: S105
SERVICE_AUTH_HEADERS = {"X-Nyx-Service-Token": TEST_SERVICE_TOKEN}
