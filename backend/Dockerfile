FROM golang:1.20-alpine AS builder

WORKDIR /app

COPY go.mod ./
# Copy all source files
COPY . .

RUN go build -o main main.go

FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/main .

EXPOSE 8080
CMD ["./main"]
