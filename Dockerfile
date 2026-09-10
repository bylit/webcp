# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go test ./... && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/webcp ./cmd/webcp

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 10001 webcp && \
    adduser -S -D -H -u 10001 -G webcp webcp && \
    mkdir -p /data/downloads /data/.webcp && \
    chown -R webcp:webcp /data

COPY --from=build /out/webcp /usr/local/bin/webcp

USER webcp
VOLUME ["/data"]
EXPOSE 8080

ENV LISTEN_ADDR=:8080 \
    DOWNLOAD_DIR=/data/downloads \
    STATE_FILE=/data/.webcp/downloads.json \
    MAX_CONCURRENT_DOWNLOADS=4

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["webcp"]
