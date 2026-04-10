# Nyx Roadmap: From Minimalist to Production-Grade

This document outlines the planned improvements to transition Nyx from a minimalist media rating application to an advanced, enterprise-ready system.

## 1. Reliability & Observability (Ops)
Advanced systems must be observable and handle shutdowns gracefully.
- [x] **Structured Logging**: Replace standard `log` with `rs/zerolog` or `uber-go/zap` for JSON-formatted logs.
- [x] **Metrics**: Implement a `/metrics` endpoint using `prometheus/client_golang` for real-time monitoring.
- [x] **Graceful Shutdown**: Implement `context` and signal handling (`SIGTERM`, `SIGINT`) in the Go backend to finish active requests before exiting.
- [x] **Health Checks**: Expand `/api/health` to check database connectivity status beyond just the API being "up."

## 2. API Maturity & Security
Move beyond basic endpoints to a robust, documented API.
- [x] **OpenAPI/Swagger**: Integrate `swaggo/swag` to auto-generate documentation and a Swagger UI.
- [x] **Authentication**: Implement JWT-based authentication for movie creation, editing, and deletion.
- [x] **Project Restructuring (Clean Architecture)**: Move from a single-file script to a modular, domain-driven structure for better maintainability.
- [x] **Rate Limiting**: Add middleware to prevent API abuse.
- [x] **Middleware Stack**: Refactor routing to use a proper middleware chain for CORS, Logging, and Recovery.
- [x] **Standardized Error Responses**: Implement consistent JSON error formats across all endpoints.

## 3. Database Lifecycle Management
Ensure schema changes are trackable and safe.
- [x] **Migration Tooling**: Replace the `initDB()` function with a migration engine like `golang-migrate` or `pressly/goose`.
- [x] **Audit Fields**: Add `created_at`, `updated_at`, and `deleted_at` (soft deletes) to all tables.
- [x] **Integration Testing**: Implemented test infrastructure using `testcontainers-go` for real PostgreSQL instances during tests.
- [x] **Connection Pooling**: Fine-tuned PostgreSQL connection pool settings via environment variables (Viper).

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

## 5. Developer Experience (DX) & CI/CD
Automate quality control and deployment.
- [x] **GitHub Actions**: Create a CI pipeline to run `go test` and `npm test` on every pull request.
- [x] **E2E Testing**: Implemented Playwright end-to-end tests for critical user journeys.
- [x] **Backend Linting**: Integrated `golangci-lint` into the CI/CD pipeline for Go code quality and security checks.
- [x] **Frontend Linting**: Tightened `eslint` rules and integrated `husky` pre-commit hooks with `lint-staged`.
- [x] **Kubernetes Manifests**: Draft `Deployment`, `Service`, and `Ingress` YAMLs for seamless production deployment.
- [x] **Environment Configuration**: Use a more robust configuration loader (like `spf13/viper`) for the backend.

---
*Nyx: Minimalist by design, powerful by choice.*
