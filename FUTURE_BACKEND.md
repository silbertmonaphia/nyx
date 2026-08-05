# Nyx Backend: Advanced Industry Standards Roadmap

This document outlines the architectural and technical evolution of the Nyx backend, moving from a minimalist script to a high-performance, maintainable, and secure enterprise-grade API.

## 1. Core Framework & Architecture
Advanced projects prioritize scalability and separation of concerns through modular design.
- [x] **Refactor to Gin Gonic**: Replace standard `net/http` for better routing, middleware management, and JSON binding performance.
- [x] **Project Restructuring (Clean Architecture)**:
  ```text
  backend/
  ├── cmd/api/          # Entry point
  ├── internal/
  │   ├── movie/        # Movie domain logic
  │   │   ├── handler/  # API endpoints
  │   │   ├── service/  # Business logic
  │   │   └── repository/# Database interactions
  │   ├── middleware/   # Shared middlewares (Auth, Logging)
  │   └── platform/     # Database, Logger, etc.
  └── pkg/              # Public libraries
  ```

## 2. Validation & Error Handling
Never trust the client. Implement robust validation at the entry point.
- [x] **Struct-Based Validation**: Use `go-playground/validator` with struct tags (e.g., `validate:"required,min=1,max=100"`).
- [x] **Standardized Error Responses**: Implement a global error handler that returns consistent JSON structures:
  ```json
  {
    "error": "Validation Failed",
    "details": { "title": "is required" },
    "code": 400
  }
  ```
- [ ] **API Error Translators**: Implement an error mapping layer to catch database-specific constraint errors (e.g. duplicate username) and return user-friendly, semantic error messages instead of raw DB error details.

## 3. Security & Authentication
Secure the API against unauthorized access.
- [x] **JWT Authentication**: Implement JSON Web Tokens for secure session management.
- [x] **User Management**: Created a `users` table with hashed passwords using `bcrypt`.
- [x] **Auth Middleware**: Protect write/delete routes while keeping read routes public (or as configured).
- [x] **Rate Limiting**: Implemented token bucket algorithm middleware to prevent API abuse.
- [ ] **CORS Hardening**: `cors.go` currently allows `Access-Control-Allow-Origin: *`. Restrict to a configurable allowlist of origins in production.
- [ ] **JWT Secret via Config**: `jwt.go` reads the secret directly via `os.Getenv` instead of using the Viper `cfg` struct. Consolidate to use the centralized config loader.
- [ ] **JWT Refresh Tokens**: Current tokens expire in 24h with no refresh flow. Add a refresh token endpoint and short-lived access tokens for better session security.

## 4. Database Layer Enhancement
Improve data safety and developer speed.
- [x] **Type-safe SQL layer**: Migrated from `sqlx` + `lib/pq` to [sqlc][sqlc] + `pgx/v5` + `pgxpool`. SQL queries now live in `backend/queries/*.sql`; generated code in `internal/{movie,user}/db/` is regenerated via `make sqlc`. See `backend/SQLC.md`.
- [x] **Transaction Management**: Ensure atomic operations for complex logic.
- [x] **Connection Pooling**: Tune PostgreSQL connection pool settings for production loads via environment variables.
- [x] **Database Index Optimization**: Analyze access patterns and optimize PostgreSQL indexes for queries/filtering.
- [x] **Caching Layer**: Integrate Redis or an in-memory cache for read-heavy resources to minimize database lookup times.

[sqlc]: https://docs.sqlc.dev/

## 5. Observability & Documentation
Make the system transparent and easy to integrate with.
- [x] **Swagger (OpenAPI 3.0)**: Use `swaggo/swag` to auto-generate interactive API documentation.
- [x] **Prometheus Metrics**: Export latency, error rates, and request counts via a `/metrics` endpoint.
- [x] **Contextual Logging**: Pass `context` through layers to trace requests and include Request IDs in logs.
- [ ] **Distributed Tracing**: Integrate OpenTelemetry (OTel) to trace HTTP requests across router middlewares and down to individual database queries.

## 6. Configuration & Environment
- [x] **Viper Configuration**: Use `spf13/viper` for multi-source configuration (env, .yaml, .env).
- [x] **Graceful Shutdown**: Ensured background tasks and database connections are closed correctly on exit.
- [ ] **Docker Volume Guardrail**: Add developer checks or tooling to handle PostgreSQL major version upgrades/downgrades gracefully (e.g. detect incompatibilities between PG 15 and 17 and warn/auto-prune volumes).

## 7. Quality Assurance
- [x] **Unit Testing (Core)**: Implemented tests for handlers and services using `sqlmock`.
- [x] **Integration Testing**: Implemented test infrastructure using `testcontainers-go` to run real PostgreSQL instances during tests.
- [x] **GolangCI-Lint**: Integrated a strict linting pipeline (revive, gosec, staticcheck) into GitHub Actions.
- [ ] **Fix CI Integration Tests**: `ci.yml` runs `go test -v ./...` without `SKIP_CONTAINERS=true`, so the `testcontainers-go` tests will panic in CI since Docker access is needed. Either add a Postgres service container or pass the skip flag.
- [x] **Error Sentinel Values**: `handler.go` compared errors by string (`err.Error() == "movie not found"`). Replaced with `movie.ErrNotFound` + `errors.Is()` checks; the handler maps the sentinel to HTTP 404.
- [ ] **User Service Tests**: The `user` domain has no unit or integration tests. Add coverage for `Register` and `Login` service methods.
- [ ] **Handler Tests for User Domain**: `user/handler.go` has no corresponding `handler_test.go`. Add tests for register/login endpoints.
