# Douban Lite

A minimalist media rating application with a Go backend and a React frontend.

## Project Structure

- `backend/`: Go 1.20 API server
- `frontend/`: React 19 + Vite 8 SPA
- `docker-compose.yml`: Database and backend service orchestration

## Getting Started

### Prerequisites

- [Docker](https://www.docker.com/)
- [Go 1.20+](https://go.dev/) (optional, for local development)
- [Node.js 20+](https://nodejs.org/) (optional, for local development)

### Running with Docker

1. Clone the repository:
   ```bash
   git clone <repository-url>
   cd claude-test
   ```

2. Start the services:
   ```bash
   docker-compose up --build
   ```

3. Access the application:
   - Frontend: `http://localhost:5173` (once started)
   - API: `http://localhost:8080/api/health`

## Backend API

| Endpoint | Method | Description |
| --- | --- | --- |
| `/api/health` | `GET` | API health check |
| `/api/movies` | `GET` | List available movies |

## Testing

### Backend
```bash
cd backend
go test -v ./...
```

### Frontend
(Work in progress - Vitest setup pending)
