# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/akkvstore ./cmd/akkvstore

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 akkv \
    && adduser -S -D -H -u 10001 -G akkv akkv \
    && mkdir -p /data \
    && chown -R akkv:akkv /data

ENV AKKV_DATA_DIR=/data \
    AKKV_LISTEN=:8080 \
    TZ=Asia/Shanghai

COPY --from=build /out/akkvstore /usr/local/bin/akkvstore

USER akkv
EXPOSE 8080
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/akkvstore"]
