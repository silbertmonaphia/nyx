# Nyx Roadmap: From Minimalist to Production-Grade

This document outlines the planned improvements to transition Nyx from a minimalist media rating application to an advanced, enterprise-ready system.

## 1. Reliability & Observability (Ops)
Advanced systems must be observable and handle shutdowns gracefully.
- [x] **Structured Logging**: Replace standard `log` with `rs/zerolog` or `uber-go/zap` for JSON-formatted logs.
- [x] **Metrics**: Implement a `/metrics` endpoint using `prometheus/client_golang` for real-time monitoring.
- [x] **Graceful Shutdown**: Implement `context` and signal handling (`SIGTERM`, `SIGINT`) in the Go backend to finish active requests before exiting.
- [x] **Health Checks**: Expand `/api/health` to check database connectivity status beyond just the API being "up."
- [ ] **Distributed Tracing**: Integrate OpenTelemetry (OTel) to trace requests across the API and database layers.

## 2. API Maturity & Security
Move beyond basic endpoints to a robust, documented API.
- [x] **OpenAPI/Swagger**: Integrate `swaggo/swag` to auto-generate documentation and a Swagger UI.
- [x] **Authentication**: Implement JWT-based authentication for movie creation, editing, and deletion.
- [x] **Project Restructuring (Clean Architecture)**: Move from a single-file script to a modular, domain-driven structure for better maintainability.
- [x] **Rate Limiting**: Add middleware to prevent API abuse.
- [x] **Middleware Stack**: Refactor routing to use a proper middleware chain for CORS, Logging, and Recovery.
- [x] **Standardized Error Responses**: Implement consistent JSON error formats across all endpoints.
- [ ] **Semantic API Error Translators**: Implement an error mapping layer to catch database-specific constraint errors and return clean client-facing messages.
- [ ] **Error Sentinel Values**: Adopt typed sentinel errors across more domains (currently only `movie.ErrNotFound` uses them). Replace `err.Error()` string compares in any remaining call sites with `errors.Is()` for reliability.
- [ ] **CORS Hardening**: `cors.go` uses `Access-Control-Allow-Origin: *`. Restrict to a configurable origin allowlist for production.
- [ ] **JWT Secret via Viper Config**: `jwt.go` reads the secret via `os.Getenv` instead of the central Viper config struct — consolidate.
- [ ] **JWT Refresh Tokens**: Add a refresh token endpoint with short-lived access tokens for better session security.

## 3. Database Lifecycle Management
Ensure schema changes are trackable and safe.
- [x] **Migration Tooling**: Replace the `initDB()` function with a migration engine like `golang-migrate` or `pressly/goose`.
- [x] **Audit Fields**: Add `created_at`, `updated_at`, and `deleted_at` (soft deletes) to all tables.
- [x] **Integration Testing**: Implemented test infrastructure using `testcontainers-go` for real PostgreSQL instances during tests.
- [x] **Connection Pooling**: Fine-tuned PostgreSQL connection pool settings via environment variables (Viper).
- [x] **Caching Layer**: Integrate Redis or an in-memory cache for read-heavy resources to minimize database lookup times.
- [x] **Database Index Optimization**: Analyze access patterns and optimize PostgreSQL indexes for queries/filtering.
- [ ] **Container Version Conflict Guardrail**: Script checks to warn developers or automate Docker volume pruning when upgrading/downgrading Postgres major versions.

## 4. Modern Frontend Architecture
Improve the React developer experience and application performance.
- [x] **TypeScript Migration**: Full type safety for components, props, and API responses.
- [x] **Feature-Based Architecture**: Modular domain-driven folder structure (`src/features/`).
- [x] **Server State Management**: Replaced manual `fetch` in `useEffect` with **TanStack Query (React Query)** for automatic caching and re-fetching.
- [x] **Zod Validation**: Implemented runtime type validation for API responses and forms using `zod` and `react-hook-form`.
- [x] **Global State Management**: Implemented **Zustand** for lightweight and high-performance client state.
- [x] **Tailwind CSS Integration**: Utility-first styling for consistent design patterns.
- [x] **Global Error Handling**: React Error Boundaries and a global toast notification system.
- [x] **UI Component Library**: Integrate **Shadcn UI** or **Radix UI** for accessible, high-quality primitives.
- [ ] **Dynamic Metadata (SEO)**: Implement proper, dynamic title tags and meta descriptions per page for improved SEO.
- [ ] **Search Debounce**: `App.tsx` fires an API request on every search keystroke. Add a 300ms debounce.
- [x] **Pagination / Infinite Scroll**: The API returns all records in a single payload. Add server-side pagination.
- [x] **Optimistic Updates**: Mutations invalidate cache after success. Use TanStack Query `onMutate` for instant UI feedback.
- [x] **Loading Skeletons**: Replace the plain `"Loading movies..."` text with skeleton placeholder cards.
- [x] **Confirm Dialog Component**: `App.tsx` uses `window.confirm()` for delete — replace with an accessible modal dialog.
- [ ] **Persist Auth Token Securely**: The JWT is stored in `localStorage` via Zustand persist. Consider `httpOnly` cookies or document the XSS risk.

## 5. Developer Experience (DX) & CI/CD
Automate quality control and deployment.
- [x] **GitHub Actions**: Create a CI pipeline to run `go test` and `npm test` on every pull request.
- [x] **E2E Testing**: Implemented Playwright end-to-end tests for critical user journeys.
- [ ] **E2E in CI/CD**: The `e2e-test` job in `ci.yml` currently skips actual test execution. Wire up a Postgres service container and run `npm run test:e2e` end-to-end.
- [ ] **Fix Backend CI Integration Tests**: `ci.yml` runs `go test -v ./...` without a Docker service, causing `testcontainers-go` tests to panic. Add a Postgres service container or pass `SKIP_CONTAINERS=true`.
- [ ] **User Domain Test Coverage**: `user/handler.go` and `user/service.go` have no test files. Add unit tests for `Register` and `Login`.
- [x] **Backend Linting**: Integrated `golangci-lint` into the CI/CD pipeline for Go code quality and security checks.
- [x] **Frontend Linting**: Tightened `eslint` rules and integrated `husky` pre-commit hooks with `lint-staged`.
- [x] **Kubernetes Manifests**: Draft `Deployment`, `Service`, and `Ingress` YAMLs for seamless production deployment.
- [x] **Environment Configuration**: Use a more robust configuration loader (like `spf13/viper`) for the backend.

---
*Nyx: Minimalist by design, powerful by choice.*
