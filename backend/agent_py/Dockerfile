FROM python:3.12-slim AS base

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# Build deps for some wheels (cryptography, etc.) — keep small.
# libpq5: runtime for psycopg (a transitive dep of
# langgraph-checkpoint-postgres). Without it `import psycopg` raises
# `no pq wrapper available` in slim images.
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    curl \
    libpq5 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ghcr.io/astral-sh/uv:0.10.9 /uv /uvx /bin/

ENV UV_PROJECT_ENVIRONMENT=/opt/venv \
    UV_PYTHON_DOWNLOADS=never

WORKDIR /app

# Copy project metadata first for layer caching.
COPY pyproject.toml uv.lock ./
RUN uv sync --locked --no-install-project

COPY alembic.ini ./
COPY migrations ./migrations
COPY agent ./agent

RUN uv sync --locked --no-editable

ENV PATH="/opt/venv/bin:$PATH"

EXPOSE 9000 9001

HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
    CMD curl -fsS http://localhost:9000/healthz || exit 2

CMD ["python", "-m", "uvicorn", "agent.main:app", "--host", "0.0.0.0", "--port", "9000"]