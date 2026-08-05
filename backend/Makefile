.PHONY: sqlc sqlc-diff sqlc-version help

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

help:
	@echo "Targets:"
	@echo "  make sqlc         - regenerate internal/{movie,user}/db from queries/*.sql"
	@echo "  make sqlc-diff    - regenerate and fail if there are uncommitted changes"
	@echo "  make sqlc-version - print installed sqlc version"
