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
git clone https://github.com/PerpetualSoftware/pad.git
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
| `PAD_URL` | — | Public-facing base URL (e.g., `https://pad.example.com`). Used for invitation, password-reset, and share-link emails. **Required when `PAD_HOST=0.0.0.0`** — otherwise emailed links point at `http://0.0.0.0:port` and are unreachable to recipients. |
| `PUBLIC_URL` | — | Alternative to `PAD_URL` using the generic env-var convention. Server-side only — does not affect CLI mode, does not influence the CLI's API endpoint, and is not persisted to `config.toml`. Precedence: `PAD_URL` > `PUBLIC_URL` > constructed `http://host:port`. |
| `PAD_DATA_DIR` | `~/.pad` | Data directory for SQLite DB, logs, and config |
| `PAD_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `PAD_MODE` | `local` | Mode: `local`, `remote`, `cloud` |

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

### Password recovery (when email is not configured)

Without an email provider, the web "Forgot password" flow can't send a reset
link — the page says so and points users at the host-side recovery below.
Recover a locked-out account **from the server host** (the same trust model
as `pad auth setup` — shell access to the box):

```bash
# Print a single-use reset link (open it in a browser to choose a new password)
pad auth reset-password admin@example.com

# Or set a random temporary password, printed to the terminal (headless boxes).
# Log in with it, then change it immediately — all existing sessions are signed out.
pad auth reset-password admin@example.com --temp-password
```

This calls a loopback-only endpoint (`POST /api/v1/auth/local-reset`): it
needs no login (you're locked out, after all), but it **only** works for a
direct request from the server itself — proxied or remote requests are
refused, and it's disabled entirely in cloud mode.

Alternatively, if a user submits the web reset form, the server logs the
reset path on a non-cloud instance with no email configured:

```
password reset generated (email not configured) ... reset_path=/reset-password/<token>
```

Open `<base-url>/reset-password/<token>` to finish the reset by hand.

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

## Upgrading

Pad releases a new binary roughly weekly. Migrations run automatically at
startup — only the ones your database is missing are applied, and each one
commits atomically, so a failed migration rolls back cleanly and is retried
on the next boot.

**Only ever move forward.** A newer binary can migrate an older database; an
older binary cannot understand a newer schema. Pad enforces this with a
schema-ahead guard: if the binary finds a database that carries migrations it
doesn't ship (the signature of a downgrade — a rolled-back brew formula, an
older Docker tag, a redeployed prior binary), it **refuses to start** instead
of silently running old code against a newer schema and corrupting data.

```
database schema is newer than this pad binary: the database has N migration(s)
this binary doesn't ship (...) ... This almost always means the binary was
DOWNGRADED (e.g. brew/docker rollback) ... Upgrade pad back to a build that
includes those migrations, or ... re-run with `pad start --force`.
```

- **Recover** by reinstalling the newer binary (`brew upgrade pad`, pull the
  newer Docker tag, redeploy the newer image).
- **Override** — only if you have *intentionally* downgraded and accept the
  data-corruption risk — with `pad start --force` or `PAD_ALLOW_SCHEMA_AHEAD=1`.

### Pre-migration snapshot (SQLite)

When a SQLite-backed instance has pending migrations, Pad copies the database
file to `pad.db.pre-<version>` (next to the DB) *before* applying them. If an
upgrade goes wrong, stop the server and copy that file back over `pad.db`. It
is a convenience net, not a substitute for backups — take a real backup first
(see [backup.md](backup.md)). The copy is best-effort: if it can't be written
(read-only volume, full disk) the server logs a warning and proceeds, so keep
your own backups regardless.

PostgreSQL is not snapshotted this way — take a `pg_dump` or provider snapshot
before upgrading (see [backup.md](backup.md)).

### Recommended flow

```bash
# 1. Back up (SQLite shown; pg_dump for Postgres — see backup.md)
pad db backup -o pad-backup-$(date +%Y%m%d).db

# 2. Stop, install the new binary, restart. Migrations + the pre-migration
#    snapshot run automatically on start.
brew upgrade pad     # or: docker pull, binary download, systemctl restart pad

# 3. Verify
pad --version
curl -s http://localhost:7777/api/v1/health   # {"status":"ok"}
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
