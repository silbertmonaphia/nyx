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

## 3. Security & Authentication
Secure the API against unauthorized access.
- [x] **JWT Authentication**: Implement JSON Web Tokens for secure session management.
- [x] **User Management**: Created a `users` table with hashed passwords using `bcrypt`.
- [x] **Auth Middleware**: Protect write/delete routes while keeping read routes public (or as configured).
- [x] **Rate Limiting**: Implemented token bucket algorithm middleware to prevent API abuse.

## 4. Database Layer Enhancement
Improve data safety and developer speed.
- [x] **GORM or SQLX**: Transition to an ORM or a typed SQL builder for safer queries and easier mapping.
- [x] **Transaction Management**: Ensure atomic operations for complex logic.
- [ ] **Connection Pooling**: Tune PostgreSQL connection pool settings for production loads.

## 5. Observability & Documentation
Make the system transparent and easy to integrate with.
- [x] **Swagger (OpenAPI 3.0)**: Use `swaggo/swag` to auto-generate interactive API documentation.
- [x] **Prometheus Metrics**: Export latency, error rates, and request counts via a `/metrics` endpoint.
- [x] **Contextual Logging**: Pass `context` through layers to trace requests and include Request IDs in logs.

## 6. Configuration & Environment
- [x] **Viper Configuration**: Use `spf13/viper` for multi-source configuration (env, .yaml, .env).
- [x] **Graceful Shutdown**: Ensured background tasks and database connections are closed correctly on exit.

## 7. Quality Assurance
- [x] **Unit Testing (Core)**: Implemented tests for handlers and services using `sqlmock`.
- [x] **Integration Testing**: Implemented test infrastructure using `testcontainers-go` to run real PostgreSQL instances during tests.
- [ ] **GolangCI-Lint**: Integrate a strict linting pipeline (revive, gosec, staticcheck).
