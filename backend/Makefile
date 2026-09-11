.PHONY: sqlc sqlc-diff sqlc-version openapi openapi-diff coverage coverage-check coverage-html help

# COVERAGE_MIN is the per-package floor that `make coverage-check`
# enforces. Each package's coverage must be at or above this number;
# a regression needs a deliberate bump in this file (PR it like any
# other config change). 70% is set below the lowest non-excluded
# package (internal/user at ~74%) so day-to-day changes have
# headroom while a careless test-branch deletion still trips the
# gate. Bump it as production packages grow their test suites.
COVERAGE_MIN ?= 70

# COVERAGE_IGNORE lists packages excluded from the gate. Each entry
# must have a rationale — a regression in an unignored package is a
# CI bug, but adding a new excluded package needs a deliberate PR
# review so the gate doesn't quietly stop measuring things that
# matter. Current entries:
#   - nyx/internal/platform/database: mostly real-Postgres
#     connection wiring (pgxpool); only init/health paths are
#     unit-testable without testcontainers. Bump via integration
#     suite when one lands.
#   - nyx/internal/platform/cache: NewRedis + Close are exercised
#     only against a real Redis; miniredis-based tests can't reach
#     them. ~60% today from the in-process paths.
#   - nyx/cmd/openapi: CLI that prints the huma-generated spec;
#     34.6% today, mostly path-printing branches. Test value is low
#     — add an entry when coverage rises or scope widens.
COVERAGE_IGNORE ?= nyx/internal/platform/database nyx/internal/platform/cache nyx/cmd/openapi

# Shell with pipefail so a `go test` failure (a FAIL line, build
# error, etc.) isn't masked by the downstream awk's exit code. /bin/sh
# on Ubuntu is dash and lacks pipefail; bash is the only sane choice
# for a pipeline that needs to propagate upstream failures.
SHELL := /bin/bash
.SHELLFLAGS := -eo pipefail -c

# sqlc is a code generator that turns the queries/*.sql files into
# type-safe Go code under internal/{movie,user}/db/. Run it after any
# schema or query change, and commit the regenerated files.
sqlc:
	sqlc generate

# Verify generated code matches the committed copy. Fails if you
# changed a query/schema without re-running `make sqlc`.
sqlc-diff:
	sqlc generate
	git diff --exit-code internal/movie/db internal/user/db

sqlc-version:
	sqlc version

# Regenerate the committed OpenAPI 3.1 artifact from the huma
# operations. Needs no database — the spec comes from struct tags.
# Run it after any handler/route change, and commit ../api/openapi.json.
openapi:
	@mkdir -p ../api
	go run ./cmd/openapi ../api/openapi.json

# Verify the committed spec matches what the code generates. Fails if
# you changed an operation without re-running `make openapi`.
openapi-diff:
	@tmp=$$(mktemp) && \
		go run ./cmd/openapi "$$tmp" && \
		{ diff -u ../api/openapi.json "$$tmp" \
			|| { echo "openapi.json is out of date - run 'make openapi' and commit the result"; rm -f "$$tmp"; exit 1; }; } && \
		rm -f "$$tmp"

# Run the unit test suite and print per-package coverage as a table.
# Sets SKIP_CONTAINERS=true so local runs don't need a live Postgres
# or Redis — unit tests cover most lines anyway, and the gate below
# runs the same set on CI so the floor stays consistent across both
# surfaces. Override with `make coverage SKIP_CONTAINERS=` to include
# integration suites.
coverage:
	@SKIP_CONTAINERS=true go test -cover -count=1 ./...

# Fail CI if any package's coverage drops below $(COVERAGE_MIN)%.
# The awk script walks the `go test -cover` output line by line,
# extracts each `coverage: NN.N%` token, and:
#   - prints `ok      <pkg>  <pct>%` for passing packages
#   - prints `REGRESS <pkg>  <pct>% < <min>%` and sets failed=1 for
#     any package below the floor
# `exit failed` in the END block propagates to the Make rule, so a
# single regression fails the gate. Build failures (`go test` exits
# non-zero) also fail the pipeline via set -o pipefail in SHELLFLAGS.
coverage-check:
	@SKIP_CONTAINERS=true go test -cover -count=1 ./... 2>&1 \
		| awk -v min=$(COVERAGE_MIN) -v ignore="$(COVERAGE_IGNORE)" -f scripts/check-coverage.awk

# Write an HTML coverage report to backend/coverage.html. Open in a
# browser to drill into a specific package or uncovered branch. The
# .out profile is overwritten on each run. Both paths are .gitignore-d.
coverage-html:
	@SKIP_CONTAINERS=true go test -coverprofile=coverage.out -covermode=atomic -count=1 ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "wrote backend/coverage.html"

help:
	@echo "Targets:"
	@echo "  make sqlc           - regenerate internal/{movie,user}/db from queries/*.sql"
	@echo "  make sqlc-diff      - regenerate and fail if there are uncommitted changes"
	@echo "  make sqlc-version   - print installed sqlc version"
	@echo "  make openapi        - regenerate ../api/openapi.json from the huma operations"
	@echo "  make openapi-diff   - regenerate and fail if the committed spec is stale"
	@echo "  make coverage       - print per-package coverage as a table"
	@echo "  make coverage-check - fail if any package drops below $(COVERAGE_MIN)%"
	@echo "  make coverage-html  - write backend/coverage.html for browser viewing"