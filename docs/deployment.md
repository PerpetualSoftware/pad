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
- **Redis** carries real-time events, watch/push notifications, and the shared session-presence registry across multiple Pad instances. Optional for single-node.

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
| `PAD_REDIS_URL` | — | Redis URL for cross-instance pub/sub **and the session-presence registry**. Without Redis, SSE events, watch notifications, and session presence are all in-process only. |
| `PAD_REDIS_NAMESPACE` | — | Scopes every Redis key and channel to one installation. Set it when two Pad installations share a Redis endpoint. Unset means the historical names; a whitespace-only value is rejected at startup rather than treated as unset. |
| `PAD_SSE_MAX_CONNECTIONS` | `1000` | Maximum streaming connections **per instance**, across both `/api/v1/events` and `/api/v1/events/stream` |
| `PAD_SSE_MAX_PER_WORKSPACE` | `100` | Per-workspace maximum connections on `/api/v1/events`, **per instance** |
| `PAD_SSE_MAX_PER_USER` | `50` | Per-user maximum streaming connections across both endpoints, **per instance** |

#### Streaming connection limits

Pad has two SSE endpoints and they share one budget. `/api/v1/events` is
workspace-scoped (the web UI's activity stream); `/api/v1/events/stream` is
user-scoped (agent watch notifications, `pad watch --stream`). A held
connection costs the same goroutine, subscription and — with Redis — presence
registration whichever one opened it, so `PAD_SSE_MAX_CONNECTIONS` and
`PAD_SSE_MAX_PER_USER` bound them together. Only `PAD_SSE_MAX_PER_WORKSPACE`
is endpoint-specific, because the watch stream has no workspace to count
against.

> **Upgrading:** `PAD_SSE_MAX_CONNECTIONS` previously bounded `/api/v1/events`
> alone. It now covers both, so a tuned value may be reached sooner than
> before. The server logs the effective limits at startup
> (`Stream connection limits`). `/api/v1/events/stream` had no limit at all
> before this change; if you run many agent sessions per user, check
> `PAD_SSE_MAX_PER_USER` against your fleet size.

A refused connection is `429` with code `sse_limit_exceeded`. The CLI monitor
treats it like any other non-200 and backs off (5s, growing linearly, capped at
5 minutes), so refusal does not produce a reconnect storm.

The per-user limit applies to every caller, including ones with no user
account: a legacy workspace-scoped token or the fresh-install no-auth window is
bounded per *workspace* instead, at the same number. Two legacy tokens for the
same workspace therefore share a bucket.

**All three limits are PER INSTANCE, not deployment-wide.** They are enforced
in-process; there is no shared counter. A three-replica deployment with
`PAD_SSE_MAX_CONNECTIONS=1000` admits up to 3000 connections in total, and a
single user can hold `PAD_SSE_MAX_PER_USER` connections *on each replica*. Size
them per pod and multiply by replica count for the deployment ceiling. Watch
`pad_stream_connections_active` (per instance) rather than inferring the total
from the configured number.

#### Redis configuration notes

**One namespace per installation, or one endpoint per installation.** Every
Redis key and channel Pad uses carries `PAD_REDIS_NAMESPACE` when it is set:
`pad:<namespace>:events:…`, `pad:<namespace>:watchevents…`,
`pad:<namespace>:session:…`. When it is unset the names are the historical flat
ones, so upgrading changes nothing.

Two installations sharing one Redis endpoint **without** distinct namespaces
cross-feed notifications and merge their session-presence registries. Selecting
different logical DB numbers does *not* help — Redis pub/sub is not namespaced
by DB at all. The practical exposure is a **cloned database** (a staging
environment restored from a production dump), because delivery is filtered on
user id and user ids are per-installation UUIDs; for that case it is a genuine
cross-tenant leak.

> **Changing the namespace on a running deployment is a cutover, not a tweak.**
> Set it before going multi-installation rather than after, and take a brief
> maintenance window if you can. Three things to know:
>
> 1. **It partitions a rolling upgrade.** Replicas with the namespace set and
>    replicas without it do not share pub/sub channels, counters, or the
>    presence registry — they behave as two separate installations for as long
>    as the rollout takes. `GET /api/v1/sessions` answers differently depending
>    on which replica handles it, and a session-targeted push aimed across the
>    partition is skipped (reported honestly as `delivered_sessions: 0`, but not
>    delivered). Roll all replicas together, or accept a split for the duration.
> 2. **Rolling BACK re-creates the split** unless the namespace is unset at the
>    same time. The env var and the binary version have to move together in both
>    directions.
> 3. **Client resync is honest on the watch stream and silent on the activity
>    stream.** `pad:*watchevents_epoch` makes the notification stream detect the
>    changed id space and answer resumes with `sync_required`. The workspace
>    activity stream (`/api/v1/events`) has no equivalent: a client reconnecting
>    with a `Last-Event-ID` from the old keyspace against a fresh replay buffer
>    is treated as caught up, so it silently misses whatever happened during the
>    cutover until its next full page load. That is a pre-existing property of
>    any cold replay buffer (a replica restart does the same), not something the
>    namespace introduced — it is called out here because a namespace change is
>    the one case an operator triggers deliberately.
>
> Session-presence entries are transient — 90s TTL — and cost nothing either
> way.

Pad's Redis integration assumes a **single Redis node** — `redis://…`, not a
cluster. Key names carry no hash tags and Pad dials a non-cluster client, so a
user's presence index and their session entries would hash to different slots
and the Lua scripts would fail `CROSSSLOT`. Pointing Pad at a Redis Cluster is
not supported.

#### Redis health and metrics

`/health/ready` reports Redis in its payload but **does not gate readiness on
it** — the REST API, the web UI and every write path work with Redis down, so
failing readiness over a Redis blip would pull healthy replicas out of the load
balancer and turn a degraded feature into an outage. When Redis is unreachable
the payload carries `redis.reachable: false`, the probe error, and a `degrades`
list naming what is lost. Note what that list says about activity events: they
stop for **all** clients, not only across instances — the activity bus does not
fall back to a local fan-out when its publish fails. The block is absent entirely when no Redis is
configured.

Alert on these instead:

| Metric | Meaning |
|--------|---------|
| `pad_redis_up` | `0` when the last probe (every 15s) failed. Exported only when Redis is configured — absence means "no Redis", not "down" |
| `pad_stream_connections_active` | Held streaming connections on this instance, across both SSE endpoints — the population the limits bound |
| `pad_watchevents_sequence_gaps_total` | This instance missed notifications — a delivery fault |
| `pad_watchevents_resume_gaps_total` | Resumes this instance could not serve; each sends a client `sync_required`. The user-visible one |
| `pad_watchevents_notifications_missed_total` | How many notifications those gaps spanned |
| `pad_watchevents_notifications_dropped_total` | Received but not delivered to a local subscriber |
| `pad_watchevents_sequence_resets_total` | The Redis counter or epoch changed; replay buffers dropped |
| `pad_watchevents_receive_loop_exits_total` | Non-zero outside shutdown means an instance publishes but receives nothing |
| `pad_session_presence_failures_total` | Presence operations failing — **read the `op` label**, the consequences differ: `register`/`renew` under-report (a live session is unlisted and untargetable), `deregister` over-reports (a dead session stays listed and a push aimed at it reaches nobody), `list` returns a 503, `prune` is benign |

**Avoid an evicting `maxmemory-policy` for Pad's Redis.**
`docker-compose.prod.yml` sets `noeviction` for this reason; the plain
`docker-compose.yml` keeps `allkeys-lru` on its 64 MB dev instance, where the
consequence below is a momentary annoyance rather than a lost instruction —
change it too if you run that file in anger.

Under an evicting policy Redis may drop live session-presence entries under
memory pressure. Nothing can distinguish that from a TTL lapsing, so a
connected agent session briefly disappears from the picker and a push targeted
at it reports
`delivered_sessions: 0`. It self-repairs on the session's next 30-second
renewal, and Pad's keyspace is small — a few hundred bytes per connected
session plus two counters — so there is nothing to gain by evicting it.

#### If push stops finding a session (on-call)

The most likely Redis-related symptom is a **transient write failure while
registering a session**. The agent's event stream stays up — the connection is
never refused over a registry problem — but the session is absent from the
shared registry, so:

- it does not appear in `GET /api/v1/sessions` or the web picker, and
- a push **targeted** at it returns `200 pushed:true` with
  `delivered_sessions: 0` and skips publication, so the instruction is not
  delivered.

**What you'll see:** `session presence: failed to register session` or `failed
to renew session entry` warnings (rate-limited to one per minute, carrying
`failures_since_last_log` — a large count means the replica, a small one means
a single session), and the session missing from the listing.

**What to do:** restore Redis connectivity, capacity, or ACLs. Registration
self-heals — each session's renewal re-writes its full entry, so an affected
session reappears within ~30 seconds without reconnecting. Confirm it is listed
again before re-sending anything.

**What NOT to do:** do not blindly re-send. A *targeted* push reporting
`delivered_sessions: 0` is safe to resend, because the server skipped the
publish. A **broadcast** is always published, and a `502 push_unconfirmed` means
the outcome is unknown — re-sending either can deliver a second instruction the
agent acts on twice. Only re-send what the server told you it skipped.

#### Upgrading a multi-instance deployment

`PAD_REDIS_URL` now also backs the **session-presence registry** — the list of
which agent sessions are connected, which `pad push` and the web UI's "Push to
agent" picker read to decide where a push goes. Previously that registry was
per-process even when Redis was configured, so a push aimed at a session held
by another replica was silently dropped.

**During a rolling upgrade, old and new replicas disagree about presence.** An
old replica has only its own connections in view, so a push it answers cannot
see a session held on a new replica, and `GET /api/v1/sessions` returns a
different list depending on which replica answers. A TARGETED push reports this
honestly — `delivered_sessions: 0`, and the publish is skipped, so nothing was
sent — but the instruction is not delivered.

This is the same behaviour every replica had *before* this build, so the
rollout is not a regression; it is a window in which the fix is only partly in
effect. Two ways to avoid the window:

- **Blue/green** — bring up the new replicas, cut traffic over, retire the old
  ones. No mixed period.
- **Drain first** — scale old replicas out of the load balancer and let agent
  monitors reconnect (`pad watch --stream` reconnects on its own) before
  serving pushes from the new set.

If neither is practical, a rolling upgrade is still safe: nothing is corrupted
and no migration is needed. Targeted pushes may report `delivered_sessions: 0`
and go undelivered until every replica runs the new build; those are safe to
re-send once the rollout completes, because a targeted miss skips the publish
entirely.

**That safety does not extend to broadcasts.** A broadcast push is always
published, on old and new replicas alike, and the shared notification bus
carries it across instances regardless of which registry the answering replica
used — so a broadcast reporting `0` during the rollout may well have been
delivered. Re-sending one is a second instruction the receiving agent will act
on twice. Only re-send a push the server told you it skipped. There is no Redis or database migration; the
registry's keys are transient and expire on their own TTL.

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
| `pad_sse_connections_active` | gauge | Connections on the workspace activity stream (`/api/v1/events`) only |
| `pad_stream_connections_active` | gauge | Held connections across **both** SSE endpoints — the population the limits bound |
| `pad_eventbus_publish_total` | counter | Events published |
| `pad_eventbus_subscribers` | gauge | Active event subscribers |
| `pad_db_open_connections` | gauge | Database connection pool stats |

Redis-specific metrics are listed under [Redis health and metrics](#redis-health-and-metrics).

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
- [ ] **Redis:** Connected for multi-instance events, notifications, and session presence (`PAD_REDIS_URL`), on a non-evicting `maxmemory-policy`, single node (not a cluster)
- [ ] **Redis namespace:** `PAD_REDIS_NAMESPACE` set if this endpoint is shared with another Pad installation
- [ ] **Streaming limits:** `PAD_SSE_MAX_CONNECTIONS` / `PAD_SSE_MAX_PER_USER` sized for your fleet (both cover *both* SSE endpoints)
- [ ] **Redis alerting:** `pad_redis_up` and `pad_watchevents_sequence_gaps_total` wired to alerts
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
