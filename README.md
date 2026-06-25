# nano-proxy

A small, single-binary reverse proxy for the [NanoGPT API](https://docs.nano-gpt.com)
that records every request, extracts token / cache / price telemetry, and ships
with a built-in dashboard for per-key spend analytics.

Designed for the 512 MB / 10 concurrent-streams / 100k-token envelope.

## Features

- One static Go binary (~12 MB). No runtime dependencies on the host.
- OpenAI-compatible passthrough for `/v1/chat/completions` (stream + non-stream).
- Per-client-key spend tracking, daily rollups, and CSV export.
- Token usage, cache hits, tool-call diagnostics and latency pulled straight
  from the upstream `usage` and `x_nanogpt_pricing` fields.
- Admin dashboard embedded in the binary — no CDN, no external assets.

## Quick start

```bash
# 1. Configure secrets
cp config.example.yaml config.yaml
export NANOGPT_API_KEY="sk-..."
export ADMIN_TOKEN="$(openssl rand -hex 32)"
export ADMIN_COOKIE_SECRET="$(openssl rand -hex 32)"

# 2. Build (Go 1.22+)
go mod tidy
CGO_ENABLED=0 go build -ldflags="-s -w" -o nano-proxy ./cmd/nano-proxy

# 3. Run
./nano-proxy -config config.yaml

# 4. Open the dashboard
xdg-open http://127.0.0.1:8081/

# 5. Create a client key (returns sknp_…; show it once)
curl -X POST http://127.0.0.1:8081/admin/api/login \
  -c /tmp/jar -H 'Content-Type: application/json' \
  -d "{\"token\":\"$ADMIN_TOKEN\"}"
curl -X POST http://127.0.0.1:8081/admin/api/keys \
  -b /tmp/jar -H 'Content-Type: application/json' \
  -d '{"name":"alice-app"}'

# 6. Send a request through the proxy
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer <client_key_from_step_5>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "minimax/minimax-m2.7",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

### Smoke test

`scripts/smoke.sh` exercises the three hot paths (non-stream, stream, tools)
and verifies that `usage` and `x_nanogpt_pricing` reach the client.

```bash
CLIENT_KEY=sknp_… ADMIN_TOKEN=… ./scripts/smoke.sh
```

## Docker

The image is a static Go binary (CGO disabled, `modernc.org/sqlite` is pure-Go) on
Alpine 3.20, running as a non-root user. The whole UI is baked in via `//go:embed`,
so the runtime image only needs the binary + your `config.yaml`.

### Build & run with docker compose

```bash
cp config.example.yaml config.yaml        # edit secrets / listeners
export NANOGPT_API_KEY=sk-...
export ADMIN_TOKEN=$(openssl rand -hex 32)
export ADMIN_COOKIE_SECRET=$(openssl rand -hex 32)
docker compose up -d --build
```

The compose file:
- exposes `8080` (proxy) publicly and `8081` (admin) on `127.0.0.1` only,
- mounts `config.yaml` read-only and a named volume `nano-proxy-data` for SQLite,
- caps memory at `512m` with swap disabled,
- passes `GOMEMLIMIT=400MiB` / `GOGC=50` to the Go runtime,
- adds a `HEALTHCHECK` against `GET /healthz` on the proxy listener.

Tail logs: `docker compose logs -f nano-proxy`.
Open admin: <http://127.0.0.1:8081/login> → token from `ADMIN_TOKEN`.

### Build a single image manually

```bash
docker build -t nano-proxy:local .
docker run --rm -p 8080:8080 -p 127.0.0.1:8081:8081 \
  -v "$PWD/config.yaml:/etc/nano-proxy/config.yaml:ro" \
  -v nano-data:/home/nano/data \
  -e NANOGPT_API_KEY -e ADMIN_TOKEN -e ADMIN_COOKIE_SECRET \
  nano-proxy:local
```

### Push to GHCR

```bash
echo $CR_PAT | docker login ghcr.io -u <user> --password-stdin
docker build -t ghcr.io/<user>/nano-proxy:0.1.0 -t ghcr.io/<user>/nano-proxy:latest .
docker push ghcr.io/<user>/nano-proxy:0.1.0
docker push ghcr.io/<user>/nano-proxy:latest
```

Then on the VPS:

```bash
docker pull ghcr.io/<user>/nano-proxy:latest
# write /etc/nano-proxy/config.yaml + /etc/nano-proxy/nano-proxy.env (KEY=value lines)
docker run -d --name nano-proxy --restart=unless-stopped \
  -p 8080:8080 -p 127.0.0.1:8081:8081 \
  --memory=512m --memory-swap=512m \
  --env-file /etc/nano-proxy/nano-proxy.env \
  -v /etc/nano-proxy/config.yaml:/etc/nano-proxy/config.yaml:ro \
  -v nano-proxy-data:/home/nano/data \
  ghcr.io/<user>/nano-proxy:latest
```

## Deployment on a 512 MB VPS

```bash
sudo install -d -o root -g root -m 755 /opt/nano-proxy /etc/nano-proxy /var/lib/nano-proxy
sudo install -m 755 nano-proxy /opt/nano-proxy/
sudo install -m 600 scripts/env.example /etc/nano-proxy/env
sudo install -m 644 config.yaml     /etc/nano-proxy/config.yaml
sudo install -m 644 scripts/nano-proxy.service /etc/systemd/system/nano-proxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now nano-proxy
```

Memory tuning (already in the systemd unit):
- `GOMEMLIMIT=400MiB` tells the Go runtime the soft cap so GC runs sooner.
- `GOGC=50` trades a touch of CPU for steadier RSS under churn.
- `LimitNOFILE=65535` is required when streaming many concurrent SSE responses.

Data directory lives at `/var/lib/nano-proxy` (override via `storage.db_path`).

## What gets recorded

For every proxied `/v1/chat/completions` call we persist:

| Field | Source |
|---|---|
| `ts`, `finished_ts`, `latency_ms` | local clock |
| `api_key_id` | resolved via `Authorization: Bearer …` |
| `model`, `stream`, `status_code` | request + upstream response |
| `prompt_tokens`, `completion_tokens`, `total_tokens` | `usage.*` |
| `cached_tokens`, `cache_creation_tokens`, `cache_read_tokens` | `prompt_tokens_details.cached_tokens` and `cache_*` |
| `reasoning_tokens` | `completion_tokens_details.reasoning_tokens` |
| `cost_usd`, `payment_source` | `x_nanogpt_pricing.{cost, paymentSource}` |
| `has_tool_calls`, `tool_calls_count` | streaming or message-level `tool_calls` |
| `tool_error`, `tool_error_msg` | non-JSON `arguments`, errors in stream body |
| `error_type`, `error_message` | upstream 4xx/5xx, parse errors, aborts |
| `client_ip`, `user_agent` | `X-Forwarded-For` → `RemoteAddr` |

Rollups (`daily_stats`, `daily_key_totals`) are updated in the same transaction
so the dashboard never sees inconsistent aggregates.

## Project layout

See `PLAN.md` for the full design. The code mirrors it:

```
cmd/nano-proxy/                entrypoint, server bootstrap
internal/config/               YAML loader + env expansion
internal/ui/                   embedded dashboard assets + templates
  web/templates/               html/template pages
  web/static/                  CSS + vanilla JS + chart renderer
internal/auth/                 client-key + admin-cookie auth
internal/proxy/                SSE parser + upstream client + metrics
  proxy.go                     non-stream path
  stream.go                    SSE relay + accumulator
  metrics.go                   usage / pricing / tool extraction
internal/store/                SQLite + rollups
  sqlite.go                    open, migrate, retention
  keys.go                      client-key CRUD
  requests.go                  per-request rows + daily rollups
  analytics.go                 summary, time-series, top-N
internal/handlers/             admin JSON + UI handlers
scripts/                       smoke test + systemd unit + env template
```

## Status

All 14 steps from `PLAN.md` are implemented. Verified to compile cleanly
(`go build ./...`) and run with the included smoke test against a real
NanoGPT upstream key.