# Nyx Roadmap: From Minimalist to Production-Grade

This document outlines the planned improvements to transition Nyx from a minimalist media rating application to an advanced, enterprise-ready system.

## 1. Reliability & Observability (Ops)
Advanced systems must be observable and handle shutdowns gracefully.
- [x] **Structured Logging**: Replace standard `log` with `rs/zerolog` or `uber-go/zap` for JSON-formatted logs.
- [ ] **Metrics**: Implement a `/metrics` endpoint using `prometheus/client_golang` for real-time monitoring.
- [x] **Graceful Shutdown**: Implement `context` and signal handling (`SIGTERM`, `SIGINT`) in the Go backend to finish active requests before exiting.
- [ ] **Health Checks**: Expand `/api/health` to check database connectivity status beyond just the API being "up."

## 2. API Maturity & Security
Move beyond basic endpoints to a robust, documented API.
- [ ] **OpenAPI/Swagger**: Integrate `swaggo/swag` to auto-generate documentation and a Swagger UI.
- [ ] **Authentication**: Implement JWT-based authentication for movie creation, editing, and deletion.
- [ ] **Rate Limiting**: Add middleware to prevent API abuse.
- [ ] **Middleware Stack**: Refactor routing to use a proper middleware chain for CORS, Logging, and Recovery.

## 3. Database Lifecycle Management
Ensure schema changes are trackable and safe.
- [x] **Migration Tooling**: Replace the `initDB()` function with a migration engine like `golang-migrate` or `pressly/goose`.
- [ ] **Audit Fields**: Add `created_at`, `updated_at`, and `deleted_at` (soft deletes) to all tables.
- [ ] **Connection Pooling**: Fine-tune PostgreSQL connection pool settings for high-concurrency scenarios.

## 4. Modern Frontend Architecture
Improve the React developer experience and application performance.
- [ ] **Server State Management**: Replace manual `fetch` in `useEffect` with **TanStack Query (React Query)** for automatic caching and re-fetching.
- [ ] **Zod Validation**: Use `zod` for runtime type validation of API responses and form inputs.
- [ ] **Tailwind CSS**: Integrate Tailwind for more scalable and consistent styling patterns.
- [ ] **Global Error Handling**: Implement React Error Boundaries and a global toast notification system for API errors.

## 5. Developer Experience (DX) & CI/CD
Automate quality control and deployment.
- [ ] **GitHub Actions**: Create a CI pipeline to run `go test` and `npm test` on every pull request.
- [ ] **Linting**: Add `golangci-lint` for Go and tighten `eslint` rules for React.
- [ ] **Kubernetes Manifests**: Draft `Deployment`, `Service`, and `Ingress` YAMLs for seamless production deployment.
- [ ] **Environment Configuration**: Use a more robust configuration loader (like `spf13/viper`) for the backend.

---
*Nyx: Minimalist by design, powerful by choice.*
