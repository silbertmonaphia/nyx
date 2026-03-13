FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY main .

EXPOSE 8080
CMD ["./main"]
