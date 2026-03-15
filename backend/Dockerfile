FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY main .
COPY migrations ./migrations

EXPOSE 8080
CMD ["./main"]
