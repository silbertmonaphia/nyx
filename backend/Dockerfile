# Build stage
FROM golang:1.26.1-alpine AS build

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod and sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o main ./cmd/api

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

# Create a non-root user
RUN adduser -D -u 1000 nyx
USER nyx

WORKDIR /home/nyx/

# Copy the binary and migrations from the build stage
COPY --from=build /app/main .
COPY --from=build /app/migrations ./migrations

EXPOSE 8080

CMD ["./main"]
