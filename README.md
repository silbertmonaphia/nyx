# Douban Lite

A minimalist media rating application with a Go backend and a React frontend.

## Project Structure

- `backend/`: Go 1.20 API server
- `frontend/`: React 19 + Vite 8 SPA
- `docker-compose.yml`: Database and backend service orchestration

## Features

- **Minimalist UI**: Simple and clean movie listing.
- **Search**: Case-insensitive search by movie title.
- **Backend API**: Robust Go backend with PostgreSQL.
- **Containerized**: Easy development setup with Docker Compose.

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
   # Build the backend binary locally first (CGO_ENABLED=0 for Alpine compatibility)
   (cd backend && CGO_ENABLED=0 GOOS=linux go build -o main .)
   
   # Start all services
   sudo docker compose up --build
   ```

3. Access the application:
   - Frontend: `http://localhost:5173`
   - API: `http://localhost:8080/api/health`

## Backend API

| Endpoint | Method | Description |
| --- | --- | --- |
| `/api/health` | `GET` | API health check |
| `/api/movies` | `GET` | List available movies. Use `?q=term` for searching titles. |

## Testing

### Backend
```bash
cd backend
go test -v .
```

### Frontend
```bash
cd frontend
npm test
```
