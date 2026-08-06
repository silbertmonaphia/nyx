# SQLC workflow

This document explains how to add or change a SQL query in the Nyx backend.
The data access layer is [sqlc][sqlc]-generated; the generated code lives in
`internal/{movie,user}/db/` and is committed.

[sqlc]: https://docs.sqlc.dev/

## TL;DR

```bash
# 1. Edit a .sql file under backend/queries/
# 2. Regenerate the Go bindings
make sqlc

# 3. Use the new method from a repository, or update an existing call site.
# 4. Run the tests.
cd backend && SKIP_CONTAINERS=true go test ./...
```

`make sqlc-diff` is the same as `make sqlc` plus a `git diff --exit-code` on the
generated tree — it fails CI if a query changed but the regeneration wasn't
committed. Wire this into CI next to the existing `go test` step.

## Layout

| Path | Role |
|---|---|
| `backend/sqlc.yaml` | Generator config. One SQL block per feature package (`movie`, `user`). |
| `backend/queries/movies.sql` | All queries for the movie feature. |
| `backend/queries/users.sql` | All queries for the user feature. |
| `backend/internal/movie/db/` | Generated: `db.go`, `models.go`, `movies.sql.go`, `querier.go`. **Do not edit.** |
| `backend/internal/user/db/` | Generated: `db.go`, `models.go`, `users.sql.go`, `querier.go`. **Do not edit.** |
| `backend/migrations/*.up.sql` | Schema. sqlc reads these to type-check queries; no DDL is generated. |

The generator is configured for **pgx/v5** (`sql_package: pgx/v5`). It emits a
`Querier` interface so production code depends on the interface and tests can
inject a stub.

## Adding a query

1. **Append** a new block to the appropriate `.sql` file. Use the `--- name:
   <MethodName> :<kind>` header line — `<MethodName>` becomes the method name
   on `Querier`, `<kind>` is one of `:one`, `:many`, `:exec`, `:execrows`,
   `:batch`, `:batchone`, `:batchmany`.

   ```sql
   -- name: GetMovieByID :one
   SELECT id, title, description, rating, created_at, updated_at, deleted_at
   FROM movies
   WHERE id = @id AND deleted_at IS NULL;
   ```

   Rules:
   - Parameter names use `@name`. The generator emits a `<Method>Params` struct
     with one field per named parameter.
   - For nullable inputs, use `sqlc.narg('name')`. It returns `NULL` when the
     caller leaves the field zero-valued, which lets a single query cover the
     "no filter" and "with filter" cases — see `QueryMoviesPage` /
     `CountMovies` for the canonical pattern.
   - For non-nullable inputs, use `sqlc.arg('name')`.
   - Add the table to `sqlc.yaml`'s `schema:` list if you change the migration
     layout. The default already lists every migration under `migrations/`.

2. **Regenerate**: `make sqlc`. Commit the regenerated `internal/<feature>/db/`
   files alongside your `.sql` change.

3. **Call it**: import the generated package and use `db.New(pool).Method(ctx,
   params)` (or `qtx.Method(...)` when inside a transaction — `db.Queries` has
   a `WithTx(pgx.Tx)` method that returns a tx-bound `*Queries`).

## Optional / nullable parameters

The `sqlc.narg('query')` pattern is how the search branch of `GetAll` works:

```sql
-- name: QueryMoviesPage :many
SELECT ...
FROM movies
WHERE (
        sqlc.narg('query')::text IS NULL          -- no filter
        OR title       ILIKE sqlc.narg('query')   -- LIKE title
        OR description ILIKE sqlc.narg('query')   -- LIKE description
      )
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT  sqlc.arg('page_size')::int
OFFSET sqlc.arg('offset')::int;
```

`sqlc.narg` produces a `pgtype.Text` field. When the caller does not set it,
the SQL sees `NULL` and the `IS NULL OR ...` short-circuits the search. The
caller is responsible for wrapping the search term in `%` wildcards before
passing it in.

## Transactions

`db.Queries` (returned by `db.New(pool)`) has a built-in `WithTx(pgx.Tx) *Queries`
method. To run a query inside a transaction:

```go
tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
if err != nil { return err }
defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

qtx := db.New(pool).WithTx(tx)
items, err := qtx.QueryMoviesPage(ctx, db.QueryMoviesPageParams{...})
// ...
return tx.Commit(ctx)
```

The `movie.sqlRepository.GetAll` method uses this exact pattern so its
`SELECT` and `COUNT(*) See the same snapshot.

## Schema changes

If you add or modify a migration under `migrations/`:

1. Update `migrations/00000N_description.up.sql` (and `.down.sql`).
2. Update any `queries/*.sql` files whose columns changed.
3. Run `make sqlc` to regenerate.
4. Verify `make sqlc-diff` passes — i.e. no uncommitted generated drift.

## "Not found" semantics

- `:one` queries that match zero rows return `pgx.ErrNoRows`. Use
  `errors.Is(err, pgx.ErrNoRows)` to detect this. See
  `movie/sqlRepository.Update` for the canonical mapping (the repo translates
  the error into its own `ErrNotFound` sentinel so handlers stay decoupled
  from pgx).
- `:execrows` queries return a `pgconn.CommandTag`. Use
  `commandTag.RowsAffected() == 0` to detect zero rows. See
  `movie/sqlRepository.Delete`.

## Troubleshooting

**"package internal/movie/db: no queries found"** — sqlc found no query blocks
in the `.sql` file. Every block must begin with a `--- name: ... :<kind>`
header line.

**"column ... does not exist"** during `make sqlc` — sqlc type-checks each
query against the schema files listed in `sqlc.yaml`. Either the column
doesn't exist (you forgot a migration) or `sqlc.yaml`'s `schema:` list is out
of date.

**Generated method signature changed unexpectedly** — sqlc renames a column
when the `rename:` map in `sqlc.yaml` matches. If you rename a column in the
schema, update `rename:` too.

**`pgx.ErrNoRows` returned for a query I expected to return multiple rows** —
`:one` queries return `ErrNoRows` for zero rows AND no rows. If you want a
zero-row result to be a valid empty value, switch the query to `:many` and
return a (possibly empty) slice.

**Tests that used to pass with sqlmock now fail after migrating to pgxmock** —
pgx encodes nullable inputs as `pgtype.Text`/`pgtype.Float8` wrappers, not
raw strings or floats. Use `pgxmock.AnyArg()` for those parameters in the
test; pin only the non-nullable args and the row data.

## CI

Add a `make sqlc-diff` step after `go test`:

```yaml
- name: Verify generated code is up to date
  working-directory: backend
  run: make sqlc-diff
```

It runs `sqlc generate` and `git diff --exit-code` on
`internal/movie/db`/`internal/user/db`. A failure here means someone changed a
query without regenerating, or vice versa.