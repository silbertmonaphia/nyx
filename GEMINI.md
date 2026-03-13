# Nyx - Project Overview & Mandates

Nyx is a minimalist media rating application featuring a Go backend, a React frontend, and a PostgreSQL database, all orchestrated via Docker Compose for a seamless development experience.

## Technical Architecture

- **Backend**: Go 1.20 API server located in `backend/`.
  - Uses `github.com/lib/pq` for PostgreSQL connectivity.
  - Implements automatic database migrations and data seeding on startup.
  - Includes a retry mechanism (10 attempts, 3s delay) to wait for database readiness.
- **Frontend**: React 19 + Vite 8 SPA located in `frontend/`.
  - Uses modern functional components and Hooks (`useState`, `useEffect`).
  - Styled with custom Vanilla CSS and modern nesting.
- **Database**: PostgreSQL 15.
  - Configured via `docker-compose.yml`.
  - Host port: `5433` (mapped to internal `5432`).

## Building and Running

### Using Docker (Recommended)
The entire stack can be launched with a single command:
```bash
# Build the backend binary locally first (CGO_ENABLED=0 for Alpine compatibility)
(cd backend && CGO_ENABLED=0 GOOS=linux go build -o main .)

# Start all services
sudo docker compose up --build
```
- **Frontend**: http://localhost:5173
- **Backend API**: http://localhost:8080/api/movies
- **Health Check**: http://localhost:8080/api/health

### Local Development
- **Backend**: `cd backend && go run main.go`
- **Frontend**: `cd frontend && npm install && npm run dev`

## Testing

### Backend (Go)
Run unit tests for API handlers:
```bash
cd backend
go test -v .
```

### Frontend (Vitest)
Run component tests using Vitest and React Testing Library:
```bash
cd frontend
npm test
```

## Development Conventions

1. **Docker-First Build**: The backend `Dockerfile` uses a pre-built static binary to bypass potential network timeouts during container builds. Always build the binary locally before running `docker compose up --build`.
2. **Database Resilience**: Do not assume the database is immediately available. The backend must implement retry logic for the initial connection.
3. **CORS Handling**: The backend explicitly sets `Access-Control-Allow-Origin: *` to facilitate development with the Vite dev server.
4. **Environment Variables**: Use a `.env` file to configure the database connection string (`DB_URL`) and other credentials. See `.env.example` for details.
5. **Component Standards**: Frontend components should be functional and keep logic separated from presentation where possible.
