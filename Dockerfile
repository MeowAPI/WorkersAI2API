FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/workersai2api .

FROM alpine:3.23
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN adduser -D -u 10001 app && mkdir -p /app/data && chown -R app:app /app
WORKDIR /app
COPY --from=builder /out/workersai2api /usr/local/bin/workersai2api
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/workersai2api"]
