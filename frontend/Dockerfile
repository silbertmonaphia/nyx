# Build stage
FROM node:22-alpine AS build

WORKDIR /app

# Copy package files and install dependencies
# SECURITY.md H10: `npm ci` (deterministic, lockfile-driven) + skip the
# audit/fund network calls. `package-lock.json` is committed at repo root.
COPY package*.json ./
RUN npm ci --no-audit --no-fund

# Copy source code and build the production version
COPY . .
ARG VITE_API_URL
ENV VITE_API_URL=$VITE_API_URL
ARG VITE_OTEL_ENABLED
ARG VITE_OTEL_SERVICE_NAME
ARG JAEGER_HOST=jaeger
ARG API_ORIGIN
ENV VITE_OTEL_ENABLED=$VITE_OTEL_ENABLED \
    VITE_OTEL_SERVICE_NAME=$VITE_OTEL_SERVICE_NAME
RUN npm run build

# Production stage
FROM nginx:stable-alpine

# Use a non-root user
RUN touch /var/run/nginx.pid && \
    chown -R nginx:nginx /var/run/nginx.pid && \
    chown -R nginx:nginx /var/cache/nginx && \
    chown -R nginx:nginx /var/log/nginx && \
    chown -R nginx:nginx /etc/nginx/conf.d

USER nginx

# Copy the built files from the build stage to Nginx's static file directory
COPY --from=build /app/dist /usr/share/nginx/html

# Copy a custom Nginx configuration
COPY nginx.conf /etc/nginx/conf.d/default.conf

RUN sed -i "s|__JAEGER_HOST__|${JAEGER_HOST}|g" /etc/nginx/conf.d/default.conf && \
    sed -i "s|__API_ORIGIN__|${API_ORIGIN}|g" /etc/nginx/conf.d/default.conf

EXPOSE 8080

# SECURITY.md M11: liveness probe. We use `--spider` on `/` rather than
# a dedicated `/healthz` endpoint because the nginx config (Track 1
# doesn't add one; Track 4 may) only serves the SPA shell and the
# OTLP proxy — there is no application-level health route to point
# at. `--spider` does a HEAD against the index without writing the
# body to disk, which keeps the probe cheap on the nginx event loop.
# Sensible defaults: 30s interval (the SPA is stateless; a 200 is the
# minimum bar for "container is alive"), 10s start-period (Vite-built
# SPA + nginx boot completes well under that on the compose target),
# 5s timeout, 3 retries — three consecutive failures flips the
# container to `unhealthy` so compose / k8s can reschedule.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10s \
    CMD wget -q --spider http://localhost:8080/ || exit 1

CMD ["nginx", "-g", "daemon off;"]
