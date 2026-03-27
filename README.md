# Nyx

A minimalist media rating application with a Go backend and a React frontend.

## Project Structure

- `backend/`: Go 1.26.1 API server (Gin-ready)
- `frontend/`: React 19 + Vite 8 SPA (TanStack Query + Tailwind CSS)
- `docker-compose.yml`: General service orchestration
- `docker-compose.prod.yml`: Production-specific configuration

## Features

- **Advanced UI**: Modern React frontend with Tailwind CSS and TanStack Query.
- **Search**: Case-insensitive search by movie title.
- **Backend API**: Robust Go backend with PostgreSQL and auto-migrations.
- **TypeScript**: Full-stack type safety with TypeScript in the frontend.
- **Validation**: Runtime validation with Zod (frontend) and Go structs (backend).
- **Containerized**: Production-ready Docker builds with pre-compiled binaries.

## Getting Started

### Prerequisites

- [Docker](https://www.docker.com/)
- [Docker Compose](https://docs.docker.com/compose/)
- [Go 1.26+](https://go.dev/dl/)
- [Node.js 22+](https://nodejs.org/)

> **Tip:** To avoid using `sudo` with Docker, add your user to the docker group:
> ```bash
> sudo usermod -aG docker $USER && newgrp docker
> ```

### Development

1. Clone the repository:
   ```bash
   git clone git@github.com:silbertmonaphia/nyx.git
   cd nyx
   ```

2. Configure environment variables:
   ```bash
   cp .env.example .env
   # Edit .env if you want to change default credentials
   ```

3. Build and start the services:
   ```bash
   # Build backend binary (Alpine requires static binary)
   (cd backend && CGO_ENABLED=0 go build -o main ./cmd/api)
   
   # Start all services
   sudo docker compose up --build -d
   ```

4. Access the application:
   - Frontend: `http://localhost:5173` (with HMR)
   - API: `http://localhost:8080/api/movies`
   - Swagger UI: `http://localhost:8080/api/swagger/index.html`

### Production Deployment

To deploy Nyx in a production environment:

1. Build and run with the production configuration:
   ```bash
   # Make sure to set VITE_API_URL to your production domain if different
   VITE_API_URL=http://your-production-ip/api sudo docker compose -f docker-compose.prod.yml up -d --build
   ```

2. The application will be accessible on port 80:
   - Frontend: `http://your-production-ip`
   - API: `http://your-production-ip/api` (internal communication)

## Testing

### Backend (Go)
```bash
cd backend
go test -v ./...
```

### Frontend (Vitest)
```bash
cd frontend
npm test
```

## Production Roadmap

See `FUTURE_BACKEND.md` and `FUTURE_FRONTEND.md` for the planned enhancements towards a fully production-grade application, including JWT authentication, observability, and design system integration.
