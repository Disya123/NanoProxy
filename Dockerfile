# syntax=docker/dockerfile:1.6
#
# Multi-stage build:
#   1. builder  — golang:1.22-alpine, builds a fully static binary (CGO disabled, modernc.org/sqlite is pure-Go)
#   2. runtime  — alpine:3.20, non-root, distroless-style minimal surface, healthcheck via /healthz
#

# ---------- 1. builder ----------
FROM golang:1.22-alpine AS builder

# Toolchain hints for reproducible builds.
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /src

# Cache module downloads. Copying go.mod alone lets `go mod download` create
# go.sum on the first run (it's idempotent for unchanged go.mod).
COPY go.mod ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build the binary. Web assets are baked in via //go:embed — no external runtime files needed.
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download && \
    go build -trimpath -ldflags="-s -w" \
        -o /out/nano-proxy \
        ./cmd/nano-proxy

# ---------- 2. runtime ----------
FROM alpine:3.20 AS runtime

# CA certs for outbound HTTPS to nano-gpt.com,
# wget busybox for HEALTHCHECK,
# tzdata for sane log timestamps.
RUN apk add --no-cache ca-certificates wget tzdata

# Non-root user.
RUN addgroup -S nano && adduser -S nano -G nano

# Binary + reference config.
COPY --from=builder /out/nano-proxy /usr/local/bin/nano-proxy
COPY --from=builder /src/config.example.yaml /etc/nano-proxy/config.example.yaml

# Persistent state lives under /home/nano/data (mounted as a volume).
RUN mkdir -p /home/nano/data && chown -R nano:nano /home/nano
WORKDIR /home/nano

ENV NANOPROXY_CONFIG=/etc/nano-proxy/config.yaml
USER nano

VOLUME ["/home/nano/data"]
EXPOSE 8080 8081

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/nano-proxy"]
CMD ["--config", "/etc/nano-proxy/config.yaml"]
