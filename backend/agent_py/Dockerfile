FROM python:3.12-slim AS base

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# Build deps for some wheels (cryptography, etc.) — keep small.
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Create a venv so deps live in a single, easy-to-copy tree.
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

WORKDIR /app

# Copy project metadata first for layer caching.
COPY pyproject.toml ./
COPY agent ./agent

# Install the package itself (no editable mode in the image).
RUN pip install --upgrade pip && pip install . && pip install "httpx>=0.27"

EXPOSE 9000 9001

HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
    CMD curl -fsS http://localhost:9000/healthz || exit 2

CMD ["python", "-m", "uvicorn", "agent.main:app", "--host", "0.0.0.0", "--port", "9000"]