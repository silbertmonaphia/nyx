.PHONY: lint lint-ruff lint-mypy mypy-baseline fix help

# Shell with pipefail so `mypy` and `ruff` failures aren't masked by the
# downstream diff/grep exit code. /bin/sh on Ubuntu is dash and lacks
# pipefail; bash is the only sane choice for a pipeline that needs to
# propagate upstream failures. Mirrors backend/Makefile.
SHELL := /bin/bash
.SHELLFLAGS := -eo pipefail -c

# Run the agent lint gate (ruff + mypy with baseline diff). Same target
# as CI's `agent-lint` job — local runs should match the gate one-for-one.
# Override the executable path with `RUFF=...` / `MYPY=...` if the venv
# is somewhere non-standard.
RUFF ?= ./.venv/bin/ruff
MYPY ?= ./.venv/bin/mypy

# Ruff is a hard fail gate — zero findings required. The per-file-ignores
# in pyproject.toml handle systematic test-fixture noise.
lint-ruff:
	$(RUFF) check .

# Mypy uses a baseline diff: only NEW errors fail the gate. The current
# 43 pre-existing errors live in .mypy-known-errors.txt; fixing any of
# them requires updating the baseline (run `make mypy-baseline` after
# the fix lands, or remove the fixed line from the baseline file).
# When the baseline is fully clean, swap this for an unconditional
# `$(MYPY) agent` and delete .mypy-known-errors.txt + mypy-baseline.
lint-mypy:
	@tmp=$$(mktemp); \
	trap 'rm -f "$$tmp"' EXIT; \
	{ $(MYPY) agent 2>&1 || true; } | grep -E ' error: ' | sort -u > "$$tmp"; \
	if diff -u .mypy-known-errors.txt "$$tmp" > /dev/null; then \
		echo "mypy: no new errors vs baseline"; \
	else \
		echo "mypy errors diverged from .mypy-known-errors.txt"; \
		diff -u .mypy-known-errors.txt "$$tmp" || true; \
		echo ""; \
		echo "  new errors fail the gate."; \
		echo "  fix them, or refresh the baseline with 'make mypy-baseline' if intentional."; \
		exit 1; \
	fi

# Aggregate target: runs both gates. Wired into CI's agent-lint job.
lint: lint-ruff lint-mypy

# Regenerate the mypy baseline from the current error set. Use after
# intentionally accepting new mypy noise (e.g. adding a dependency that
# lacks type stubs). Always review the diff before committing.
mypy-baseline:
	{ $(MYPY) agent 2>&1 || true; } | grep -E ' error: ' | sort -u > .mypy-known-errors.txt
	@echo "updated .mypy-known-errors.txt ($$(wc -l < .mypy-known-errors.txt) entries)"

# Apply ruff's safe autofixes locally. CI never auto-fixes; this is
# a developer convenience. Review the diff before committing.
fix:
	$(RUFF) check --fix .

help:
	@echo "Targets:"
	@echo "  make lint          - ruff check + mypy (baseline diff); CI gate"
	@echo "  make lint-ruff     - ruff check only"
	@echo "  make lint-mypy     - mypy with .mypy-known-errors.txt baseline"
	@echo "  make mypy-baseline - regenerate .mypy-known-errors.txt from current errors"
	@echo "  make fix           - ruff --fix (developer convenience)"
