# Deployment Guide

Pad is a single Go binary with an embedded web UI. It supports SQLite (default) for single-node deployments and PostgreSQL + Redis for production multi-node setups.

## Architecture

```
                    ┌─────────────────┐
                    │  Reverse Proxy  │
                    │  (Caddy/nginx)  │
                    └────────┬────────┘
                             │ :443
                    ┌────────▼────────┐
                    │      Pad        │
                    │   Go binary     │
                    │  (web UI + API) │
                    └──┬──────────┬───┘
                       │          │
              ┌────────▼──┐  ┌───▼────────┐
              │ PostgreSQL │  │   Redis    │
              │ (storage)  │  │ (pub/sub)  │
              └────────────┘  └────────────┘
```

- **Pad** serves the REST API and embedded SvelteKit web UI on a single port (default: 7777)
- **PostgreSQL** stores all data (workspaces, items, users, activity). SQLite works for single-node.
- **Redis** enables real-time SSE events across multiple Pad instances. Optional for single-node.

## Quick Start with Docker Compose

```bash
# Clone the repo
git clone https://github.com/xarmian/pad.git
cd pad

# Start everything (Pad + PostgreSQL + Redis)
docker compose up -d

# Check status
docker compose ps

# View logs
docker compose logs -f pad
```

Access the web UI at **http://localhost:7777**. On first visit, you'll be prompted to create an admin account.

### Production Docker Compose

```bash
# Use the production overlay for resource limits and secure settings
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d
```

Edit `docker-compose.prod.yml` to set your domain, email credentials, and database password.

## Environment Variables

All configuration is via environment variables or a config file (`~/.pad/config.toml` / `/data/config.toml`).

### Core

| Variable | Default | Description |
|----------|---------|-------------|
| `PAD_HOST` | `127.0.0.1` | Listen address (`0.0.0.0` for Docker/production) |
| `PAD_PORT` | `7777` | Listen port |
| `PAD_URL` | — | Public-facing base URL (e.g., `https://pad.example.com`). Used for invitation links. |
| `PAD_DATA_DIR` | `~/.pad` | Data directory for SQLite DB, logs, and config |
| `PAD_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `PAD_MODE` | `local` | Mode: `local`, `remote`, `docker`, `cloud` |

### Database

| Variable | Default | Description |
|----------|---------|-------------|
| `PAD_DB_DRIVER` | `sqlite` | Database driver: `sqlite` or `postgres` |
| `PAD_DB_PATH` | `~/.pad/pad.db` | SQLite database path (ignored when using PostgreSQL) |
| `PAD_DATABASE_URL` | — | PostgreSQL connection string (required when `PAD_DB_DRIVER=postgres`) |

### Real-time Events

| Variable | Default | Description |
|----------|---------|-------------|
| `PAD_REDIS_URL` | — | Redis URL for cross-instance pub/sub. Without Redis, SSE events are in-process only. |
| `PAD_SSE_MAX_CONNECTIONS` | `1000` | Global maximum SSE connections |
| `PAD_SSE_MAX_PER_WORKSPACE` | `100` | Per-workspace maximum SSE connections |

### Security

| Variable | Default | Description |
|----------|---------|-------------|
| `PAD_SECURE_COOKIES` | `false` | Set `Secure` flag on session cookies (requires TLS) |
| `PAD_CORS_ORIGINS` | — | Comma-separated allowed CORS origins |

### Email (Optional)

Email enables sending workspace invitation links. Without it, users can still join via CLI invite codes.

| Variable | Default | Description |
|----------|---------|-------------|
| `PAD_MAILEROO_API_KEY` | — | Maileroo sending API key |
| `PAD_EMAIL_FROM` | `noreply@getpad.dev` | Sender email address |
| `PAD_EMAIL_FROM_NAME` | `Pad` | Sender display name |

## Deployment Options

### Single Binary (SQLite)

The simplest deployment — one binary, one file for the database.

```bash
# Download or build
make build

# Run directly
PAD_HOST=0.0.0.0 ./pad server start

# Or install as a systemd service (see below)
```

Best for: single-user, small teams, evaluations.

### Docker Compose (PostgreSQL + Redis)

See [Quick Start](#quick-start-with-docker-compose) above. This is the recommended setup for teams.

### Kubernetes

Manifests are in `deploy/k8s/`. Apply them in order:

```bash
# Create namespace
kubectl apply -f deploy/k8s/namespace.yaml

# Configure secrets (edit first!)
kubectl apply -f deploy/k8s/secret.yaml

# Deploy
kubectl apply -f deploy/k8s/configmap.yaml
kubectl apply -f deploy/k8s/deployment.yaml
kubectl apply -f deploy/k8s/service.yaml
kubectl apply -f deploy/k8s/ingress.yaml
kubectl apply -f deploy/k8s/hpa.yaml
```

**Prerequisites:**
- External PostgreSQL (e.g., AWS RDS, Cloud SQL, managed PG)
- External Redis (e.g., ElastiCache, Memorystore)
- Ingress controller (nginx-ingress or similar)
- TLS certificates (cert-manager recommended)

### Systemd Service

```ini
# /etc/systemd/system/pad.service
[Unit]
Description=Pad
After=network.target postgresql.service redis.service

[Service]
Type=simple
User=pad
Group=pad
ExecStart=/usr/local/bin/pad server start
Environment=PAD_HOST=0.0.0.0
Environment=PAD_DATA_DIR=/var/lib/pad
Environment=PAD_DB_DRIVER=postgres
Environment=PAD_DATABASE_URL=postgres://pad:secret@localhost:5432/pad
Environment=PAD_REDIS_URL=redis://localhost:6379
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now pad
```

## Reverse Proxy

Pad needs a reverse proxy for TLS termination. SSE connections require specific proxy settings to avoid buffering.

### Caddy (Recommended)

Caddy handles TLS automatically. See `deploy/Caddyfile`:

```
pad.example.com {
    reverse_proxy pad:7777 {
        flush_interval -1
    }
}
```

### nginx

See `deploy/nginx.conf`. Critical settings for SSE:

```nginx
location /api/v1/events {
    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 86400s;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
}
```

## Monitoring

Pad exposes Prometheus metrics at `/metrics` (unauthenticated). Key metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `pad_http_requests_total` | counter | Total HTTP requests by method, path, status |
| `pad_http_request_duration_seconds` | histogram | Request latency |
| `pad_http_response_size_bytes` | histogram | Response body sizes |
| `pad_sse_connections_active` | gauge | Current SSE connections |
| `pad_eventbus_publish_total` | counter | Events published |
| `pad_eventbus_subscribers` | gauge | Active event subscribers |
| `pad_db_open_connections` | gauge | Database connection pool stats |

### Health Check

```bash
curl http://localhost:7777/api/v1/health
# {"status":"ok"}
```

## Production Checklist

- [ ] **Database:** PostgreSQL configured with `PAD_DB_DRIVER=postgres`
- [ ] **Redis:** Connected for multi-instance SSE (`PAD_REDIS_URL`)
- [ ] **TLS:** Reverse proxy with valid certificates
- [ ] **Secure cookies:** `PAD_SECURE_COOKIES=true` (requires TLS)
- [ ] **Public URL:** `PAD_URL` set to your public-facing domain
- [ ] **CORS:** `PAD_CORS_ORIGINS` set if serving from a different domain
- [ ] **Backups:** PostgreSQL backup strategy in place (see `docs/backup.md`)
- [ ] **Monitoring:** Prometheus scraping `/metrics`
- [ ] **Admin account:** Created via `pad auth setup` or web UI on first visit
- [ ] **Email (optional):** Maileroo configured for invitation emails
- [ ] **Resource limits:** Set in Docker Compose or K8s manifests
- [ ] **Log level:** `PAD_LOG_LEVEL=info` (use `debug` only for troubleshooting)
