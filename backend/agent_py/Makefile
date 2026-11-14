.PHONY: lint lint-ruff lint-mypy test test-cov fix help

# Shell with pipefail so `mypy` and `ruff` failures aren't masked by a
# downstream exit code. /bin/sh on Ubuntu is dash and lacks pipefail;
# bash is the only sane choice for recipes that need to propagate
# upstream failures. Mirrors backend/Makefile.
SHELL := /bin/bash
.SHELLFLAGS := -eo pipefail -c

# Run the agent lint gate (ruff + mypy, both hard gates). Same target
# as CI's `agent-lint` job — local runs should match the gate one-for-one.
# Override the executable path with `RUFF=...` / `MYPY=...` if the venv
# is somewhere non-standard.
RUFF ?= ./.venv/bin/ruff
MYPY ?= ./.venv/bin/mypy
PYTHON ?= ./.venv/bin/python

COVERAGE_MIN ?= 90

# Ruff is a hard fail gate — zero findings required. The per-file-ignores
# in pyproject.toml handle systematic test-fixture noise.
lint-ruff:
	$(RUFF) check .

lint-mypy:
	$(MYPY) agent

# Aggregate target: runs both gates. Wired into CI's agent-lint job.
lint: lint-ruff lint-mypy

test:
	$(PYTHON) -m pytest tests/ -q

test-cov:
	@if [ -z "$$TEST_ASYNC_PG_DSN" ] || [ -z "$$PYTHON_AGENT_DATABASE_URL" ]; then \
		echo "TEST_ASYNC_PG_DSN and PYTHON_AGENT_DATABASE_URL are required: the coverage floor (COVERAGE_MIN=$(COVERAGE_MIN), recorded in backend/agent_py/Makefile) is measured against the DB-backed suite."; \
		exit 1; \
	fi
	@echo "Coverage floor: $(COVERAGE_MIN)% (recorded in backend/agent_py/Makefile)"
	$(PYTHON) -m pytest tests/ -q -rs --cov=agent --cov-report=term-missing --cov-fail-under=$(COVERAGE_MIN)

# Apply ruff's safe autofixes locally. CI never auto-fixes; this is
# a developer convenience. Review the diff before committing.
fix:
	$(RUFF) check --fix .

help:
	@echo "Targets:"
	@echo "  make lint          - ruff check + mypy; CI gate"
	@echo "  make lint-ruff     - ruff check only"
	@echo "  make lint-mypy     - mypy (hard gate, zero errors)"
	@echo "  make test          - full pytest suite (no coverage)"
	@echo "  make test-cov      - full suite with coverage floor (needs TEST_ASYNC_PG_DSN + PYTHON_AGENT_DATABASE_URL)"
	@echo "  make fix           - ruff --fix (developer convenience)"
