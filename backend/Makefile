.PHONY: sqlc sqlc-diff sqlc-version openapi openapi-diff help

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

help:
	@echo "Targets:"
	@echo "  make sqlc         - regenerate internal/{movie,user}/db from queries/*.sql"
	@echo "  make sqlc-diff    - regenerate and fail if there are uncommitted changes"
	@echo "  make sqlc-version - print installed sqlc version"
	@echo "  make openapi      - regenerate ../api/openapi.json from the huma operations"
	@echo "  make openapi-diff - regenerate and fail if the committed spec is stale"
