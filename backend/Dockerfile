# Build stage
FROM golang:1.26.1-alpine AS build

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod and sum files
COPY go.mod go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN GOPROXY=${GOPROXY} go mod download

# Copy source code
COPY . .

# Build the binary.
#
# Reproducible-build flags (H11):
#   -trimpath       strips local filesystem paths from the binary so the
#                   build output is identical regardless of where it was
#                   compiled (no /home/.../cmd/api, no $GOPATH leakage).
#   -ldflags="-s -w -buildid="
#       -s          omit the symbol table and debug info.
#       -w          omit the DWARF debugging information.
#       -buildid=   empty the build ID so builds are byte-identical
#                   between rebuilds (only source + flags vary).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -buildid=" -o main ./cmd/api

# Runtime stage.
#
# Pinned to alpine 3.20 by digest (H9). The digest below was pulled from
# Docker Hub for the official library/alpine:3.20 (amd64) image; pinning
# protects against a future tag re-point silently changing the base image.
# Re-verify the digest whenever alpine 3.20 is bumped.
#   digest ref: https://hub.docker.com/_/alpine/tags?name=3.20
#   alpine 3.20 (amd64) sha256: c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e
FROM alpine:3.20@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e

RUN apk --no-cache add ca-certificates wget

# Create a non-root user.
# uid 1000 is conventional for "first non-system user"; matching the host
# user avoids bind-mount permission surprises in dev. nyx needs network
# egress (wget) for HEALTHCHECK; no extra caps required.
RUN adduser -D -u 1000 nyx
USER nyx

WORKDIR /home/nyx/

# Copy the binary and migrations from the build stage
COPY --from=build --chown=nyx:nyx /app/main .
COPY --from=build --chown=nyx:nyx /app/migrations ./migrations

EXPOSE 8080

# Liveness probe (M11). wgets the public /api/health endpoint and treats
# any HTTP 2xx as healthy. This is intentionally a "process up + serving
# HTTP" check, not a deep dependency check (DB/Redis are not probed
# here — a deep check would amplify outages and require secret-bearing
# credentials in the health path). If you need a deep check, mount a
# separate /readyz probe and wire it to compose/K8s readiness instead.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/api/health >/dev/null 2>&1 || exit 1

CMD ["./main"]
