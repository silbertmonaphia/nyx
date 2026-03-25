# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Copy the compiled binary from your local host (built in the previous step)
COPY main .
# Copy migrations
COPY migrations ./migrations

EXPOSE 8080

CMD ["./main"]
