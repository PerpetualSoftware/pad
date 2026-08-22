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
| `PAD_EVENTS_PUBLISH_EPOCH` | `false` | Phase 2 of the event ID-space migration: publish the `<epoch>\|<id>\|<json>` wire form. **Only set this once every instance runs a binary that accepts it** — see *Event ID-space migration* below. Ignored without Redis. |

#### Streaming connection limits

Pad has two SSE endpoints and they share one budget. `/api/v1/events` is
workspace-scoped (the web UI's activity stream); `/api/v1/events/stream` is
user-scoped (agent watch notifications, `pad watch --stream`). A held
connection costs a goroutine and a bus subscription whichever one opened it —
and, on the watch stream only, a session-presence registration in shared Redis —
so `PAD_SSE_MAX_CONNECTIONS` and `PAD_SSE_MAX_PER_USER` bound them together. Only `PAD_SSE_MAX_PER_WORKSPACE`
is endpoint-specific, because the watch stream has no workspace to count
against.

> **Upgrading:** `PAD_SSE_MAX_CONNECTIONS` previously bounded `/api/v1/events`
> alone. It now covers both, so a tuned value may be reached sooner than
> before. The server logs the effective limits at startup
> (`Stream connection limits`). `/api/v1/events/stream` had no limit at all
> before this change; if you run many agent sessions per user, check
> `PAD_SSE_MAX_PER_USER` against your fleet size.

A refused connection is `429` with code `sse_limit_exceeded` and a `Retry-After`
header. The CLI monitor (`pad watch --stream`) treats it like any other non-200
and backs off (5s, growing linearly, capped at 5 minutes), so refusal does not
produce a reconnect storm. `pad project watch` is interactive and exits with an
actionable message instead.

> **Browsers do not back off.** The web UI's activity stream uses `EventSource`,
> which retries on its own fixed schedule and cannot see the status code or the
> `Retry-After` header — so a refused browser tab reconnects roughly every few
> seconds until capacity frees up. Size `PAD_SSE_MAX_CONNECTIONS` with that in
> mind: reaching it does not shed load from browser clients the way it does from
> the CLI. Tracked as BUG-2733.

The per-user limit applies to every caller. On `/api/v1/events`, callers with no
resolved user — a legacy workspace-scoped token, or the fresh-install window
before the first admin exists — are bounded per *workspace* instead, at the same
number, so two legacy tokens for one workspace share a bucket.
`/api/v1/events/stream` has no equivalent case: it requires a resolved user and
answers `401` without one.

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
different logical DB numbers only half helps: ordinary keys are DB-scoped, so
the presence registries stay separate — but Redis pub/sub is not namespaced by
DB at all, so both buses cross-feed regardless. The practical exposure is a **cloned database** (a staging
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
> 3. **Client resync is honest on both streams**, with one documented edge.
>    Each answers a resume whose cursor belongs to the old keyspace with
>    `sync_required`, by way of its cold replay-buffer coverage check rather
>    than an epoch comparison — a freshly namespaced bus has no old epoch to
>    compare against. Expect a burst of client reconciliation as they reconnect
>    — an incremental `/changes` delta each, not a full page load;
>    that is the cutover being paid for, and it is bounded by the number of
>    reconnecting clients — each RESUME is counted, so a client that
>    reconnects several times counts several times.
>
>    The edge: a cursor that lands exactly one below the first ID a replica
>    sees in the new keyspace is served rather than refused, because nothing in
>    an integer cursor distinguishes the two keyspaces. It is narrow — that one
>    value, on a client that reconnects before the replica has seen anything
>    else — and closing it needs the ID space's identity to reach the client.
>    The SSE spec would allow that (an event ID is arbitrary UTF-8); what
>    excludes it is Pad's own `id:` contract, an int64 that every deployed
>    client already parses. Tracked as BUG-2736. A maintenance window
>    narrows it — clients reconnect against an already-cut-over instance rather
>    than racing the cutover — but does not remove it, since their stored
>    `Last-Event-ID` values still belong to the old keyspace and the wire
>    format still cannot say which one they came from.
>
>    Before BUG-2731 the activity stream (`/api/v1/events`) was the silent one:
>    a client reconnecting with a `Last-Event-ID` from the old keyspace against
>    a fresh replay buffer was treated as caught up and silently missed
>    everything that happened during the cutover, until its next full page
>    load. If you are running a build older than that fix, the old behaviour
>    still applies and a namespace change wants a maintenance window rather
>    than a live cutover.
>
> Session-presence entries are transient — 90s TTL — and cost nothing either
> way.

Pad's Redis integration assumes a **single Redis node** — `redis://…`, not a
cluster. Key names carry no hash tags and Pad dials a non-cluster client, so a
user's presence index and their session entries would hash to different slots
and the Lua scripts would fail `CROSSSLOT`. Pointing Pad at a Redis Cluster is
not supported.

#### Redis health and metrics

`/api/v1/health/ready` reports Redis in its payload but **does not gate readiness on
it** — the REST API, the web UI and every item-writing path work with Redis down, so
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
| `pad_watchevents_resume_gaps_total` | Resumes this instance could not serve — from a hole, a cold start, an epoch change, or a shared-counter disagreement. Each sends a client `sync_required`, so this is the user-visible one |
| `pad_watchevents_notifications_missed_total` | How many notifications those gaps spanned |
| `pad_watchevents_notifications_dropped_total` | Received but not delivered to a local subscriber |
| `pad_watchevents_sequence_resets_total` | The Redis counter or epoch changed; replay buffers dropped |
| `pad_watchevents_receive_loop_exits_total` | Non-zero outside shutdown means an instance publishes but receives nothing |
| `pad_event_resume_gaps_total` | The ACTIVITY stream's (`/api/v1/events`) twin of the watch counter above. **Expect a step around a deploy, with the RATE settling back to baseline** (the counter itself only ever increases) — each instance starts with no replay coverage, so an early resume against a workspace it has not seen yet is a warranted resync. It counts RESUMES, not clients: a deploy with no reconnects does not move it at all, and a client that reconnects several times is counted several times. A rate that does not settle is the thing to alert on |
| `pad_event_sequence_resets_total` | Activity replay coverage dropped, by reason. `subscription_resumed` — a pub/sub connection dropped and resubscribed, dropping that workspace's buffer; expect it during a Redis failover and expect it to stop afterwards. `epoch_change` — the shared counter's ID space changed generation, dropping every buffer; expect a handful per cutover. `counter_backward` — an ID arrived at or below a buffer's high-water mark with no generation change; see *Event ID-space migration* for what to expect per phase. `epoch_regressed` — the generation counter went backwards and stayed there, which means Redis lost writes; expect zero, investigate any. `undecodable_message` — a message on these channels could not be parsed, so that workspace's coverage ended; expect zero, and suspect a namespace collision |
| `pad_event_receive_loop_exits_total` | A workspace's activity subscription loop stopped. Unlike the watch stream's twin this does **not** stay at zero — it is expected at shutdown and whenever a workspace's last local subscriber leaves. Read it as a rate against a stable subscriber count |
| `pad_session_presence_failures_total` | Presence operations failing — **read the `op` label**, the risks differ and run in opposite directions: `register`/`renew` may under-report (a live session unlisted and untargetable), `deregister` may over-report (a dead session left listed, and a push aimed at it reaches nobody), `list` returns a 503, `prune` is benign. A failure means the operation reported an error — Redis can fail a pipeline after applying it, so the write may have landed anyway |

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

#### Event ID-space migration (`PAD_EVENTS_PUBLISH_EPOCH`)

Events on the workspace activity stream (`GET /api/v1/events`) carry a
`Last-Event-ID` so a reconnecting client can be replayed what it missed. With
Redis, every instance shares one counter, so those IDs are meaningful across
replicas.

**The problem this migration fixes.** If that shared counter is ever reset —
the key evicted under `maxmemory`, deleted by hand, a fresh Redis after a
restore — IDs start again from 1. A replica that was buffering the old
sequence cannot tell the new 101 from the old 101, so it can merge two ID
spaces into one replay buffer and answer a resume across the boundary as
though nothing was missed. Numeric detection alone cannot see it: by the time
the new sequence passes the replica's high-water mark, it looks like ordinary
progress.

**What the fix does and does not close, stated before the procedure.** It
stops a REPLICA from mixing two ID spaces in one replay buffer, which is what
turns a counter reset into a silently wrong replay. It does NOT make a
CLIENT'S CURSOR say which space it came from — that would change the wire
format every deployed browser speaks. So this is a substantial mitigation and
not a closure; the residual case and why it is deferred are at the end of this
section.

The fix gives each ID space an **epoch** — a monotonic generation number,
minted by Redis when the space is created and carried as a
`<epoch>|<id>|<json>` prefix on every message published **by a phase-2
instance**. Phase-1 instances publish the historical bare JSON and carry no
epoch at all, which is what the two phases are about. A replica that sees a HIGHER
generation drops its replay buffers and answers resumes across the change with
`sync_required`, which is honest rather than silent. A message carrying a
LOWER generation is a straggler from a space that has been abandoned, and is
discarded rather than delivered.

The generation is a number rather than an opaque token so the two spaces can
be ORDERED. Workspaces have independent subscriptions and Redis does not order
messages across channels, so a pre-rotation message on one channel can arrive
after a post-rotation message on another; with an unordered token that is
indistinguishable from a second rotation.

**It rolls out in two phases, and the order is not optional.**

| Phase | What you do | What instances publish | What they accept |
|-------|-------------|------------------------|------------------|
| 1 | Roll the new binary everywhere. Leave `PAD_EVENTS_PUBLISH_EPOCH` unset. | The historical bare JSON | Both forms |
| 2 | Set `PAD_EVENTS_PUBLISH_EPOCH=true` and roll again. | `<epoch>\|<id>\|<json>` | Both forms |

The asymmetry that makes two phases necessary: an instance running a
**pre-phase-1** binary cannot parse a prefixed payload at all. It fails to
unmarshal the message and drops the event for its own clients. So flipping
before every instance is upgraded loses events on the ones that are not — not
a resync, a silent loss.

Both rolls are zero-loss in the other direction, because accept-both is on
from phase 1: during the phase-2 roll, flipped and un-flipped instances are
publishing different forms at the same time and every instance reads both.

**Rolling back to phase 1** is safe: make the effective value **false** and
roll. Peers accept the bare form throughout, so there is no window where this
direction loses events.

Two things about rolling back that are easy to get wrong:

- **Setting the value to false is not the same as unsetting the environment
  variable.** `events_publish_epoch` can also be set in `~/.pad/config.toml`,
  and the config file's value stands when the environment variable is absent.
  Clear both, or set the environment variable explicitly to `false`.
- **Downgrading past phase 1 is a SECOND step, and the order is the reverse of
  the upgrade.** A pre-phase-1 binary cannot parse the prefixed form. So:
  first roll every instance to phase 1 (new binary, flip off) and let the roll
  finish, *then* downgrade the binary. Introducing an old binary while any
  flipped instance is still publishing drops events on the old one — the same
  asymmetry that makes the upgrade two phases, in reverse.

There is no Redis or database migration in either direction. The epoch key and
its generation counter are created by the first flipped publisher; a phase-1 instance deletes it if it
ever sees the sequence counter restart, so a counter that is reset while the
deployment sits on phase 1 does not leave a stale epoch for a later phase 2 to
adopt.

**What you should see when phase 2 lands.** A replica learns the epoch from
the first prefixed message it RECEIVES — which means only replicas currently
subscribed to a workspace see it, and only when that workspace next has
traffic. If such a replica had already buffered un-prefixed events, it drops
its buffers once, records
`pad_event_sequence_resets_total{reason="epoch_change"}`, and clients resuming
across that moment get `sync_required` and re-fetch. A replica whose buffers
are EMPTY adopts the epoch without dropping anything and without a reset
count, deliberately: otherwise every replica would report a reset at startup
and the counter would grow a per-deploy baseline instead of meaning something.

Do not delete the generation counter (`<namespace>event_epoch_gen`) by hand,
and keep it out of any eviction policy: it is what makes one ID space orderable
against the next. Losing it lets a later reset reuse a generation that has
already been seen, which makes two different ID spaces look identical — the
one shape the epoch exists to prevent. It is a single small integer key; the
events keyspace should not be under `allkeys-lru` (see *Redis configuration
notes*). **One drop per replica
per roll** — if the counter keeps climbing, something is deleting the epoch or
sequence key repeatedly; check `maxmemory-policy` against the events keyspace
(see *Redis configuration notes*).

`pad_event_sequence_resets_total{reason="counter_backward"}` is the other
counter to watch. It fires when an ID arrives at or below what a buffer had
already seen.

**On phase 1 it can be non-zero at any time, not only during a roll.** Phase 1
keeps the historical two-call publish — `INCR`, then `PUBLISH` — so two
instances can interleave (INCR 5, INCR 6, PUBLISH 6, PUBLISH 5) and a receiver
sees 5 arrive after 6. That window is older than this migration; phase 2 is
what closes it, by moving ID assignment into a single atomic script so publish
order equals ID order globally.

So the expectation depends on where you are:

- **Phase 1, before this replica has ever seen a prefixed message** — expect
  ZERO. The check is deliberately not armed until an epoch has been adopted,
  because a phase-1 deployment's two-call publish interleaves as ordinary
  traffic and reacting to that would drop every replay buffer on a busy
  multi-instance deployment. The cost of that gate is that a counter reset on
  a never-flipped deployment goes undetected — which is exactly the behaviour
  before this migration existed, and precisely what phase 2 fixes.
- **During the phase-2 roll, once a replica has adopted the epoch** — expect it
  to rise for the length of the roll: un-flipped publishers are still
  assigning and publishing in two calls, and this replica is now armed.
- **Phase 2, every publisher flipped** — expect it at or near zero. A
  persistent rate here is an anomaly worth investigating rather than tuning
  away.

**Which phase an instance is publishing in is in its startup log**, as
`id_space_phase=1` or `id_space_phase=2` on the "Event bus using Redis pub/sub"
line — the counter above cannot be read without it. An unparseable
`PAD_EVENTS_PUBLISH_EPOCH` is ignored (a typo must not flip a migration whose
wrong direction loses events) and logs a warning naming the value.

**What this migration does not fix.** A client's `Last-Event-ID` is still a
bare integer with no epoch in it, and that is deliberate — every deployed
browser speaks that format, and `EventSource` echoes the header with no
application code in the path to translate it. So an old ID and a new ID of the
same numeric value remain indistinguishable **to a resume**, even though the
replica's buffers can no longer mix them. The exposure is a client that
reconnects with a cursor whose number the new sequence has already reached.
Tracked on BUG-2736.

Single-process deployments (no `PAD_REDIS_URL`) need none of this and ignore
the variable: that bus owns its counter, so it identifies its own ID space
from its start time. Two runs' IDs can only collide if the earlier process
published more than 2^20 events per millisecond of its own lifetime, or if a
restart completed inside a single millisecond — both deterministic bounds
rather than probabilities, and neither reachable by a process that has to bind
a listener and open a database before it can publish anything. A clock stepped **backwards** across a restart degrades the other
way, into extra `sync_required` responses rather than wrong replays.

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
| `pad_eventbus_publish_total` | counter | Events HANDED to the bus — attempts, not confirmed publishes. A failed Redis publish is logged and still counted (BUG-2732) |
| `pad_eventbus_subscribers` | gauge | Active event subscribers |
| `pad_db_open_connections` | gauge | Database connection pool stats |

Redis-specific metrics are listed under [Redis health and metrics](#redis-health-and-metrics).

### Health Check

Three endpoints, and they answer different questions:

```bash
# Liveness — is the process up? Kubernetes restarts the pod when this fails.
curl http://localhost:7777/api/v1/health/live
# {"status":"ok"}

# Readiness — can it serve traffic? Gated on the DATABASE only.
# Kubernetes should point its readinessProbe here.
curl -s http://localhost:7777/api/v1/health/ready
# {
#   "status": "ready",
#   "db": {"open_connections": 2, "in_use": 0, "idle": 2, "driver": "sqlite"},
#   "redis": {"reachable": true, "probed": true, "last_check": "2026-08-22T01:00:00Z"}
# }

# Build info.
curl http://localhost:7777/api/v1/health
# {"status":"ok","version":"...","commit":"..."}
```

With Redis configured but unreachable, readiness stays **200** and the `redis`
block carries the failure. Readiness deliberately does not fail: Pad still
serves the API, the web UI and every item write. What it cannot do is
cross-instance delivery, and the paths whose job that is say so —
`POST .../push` answers `503` for a session-targeted push it cannot resolve
and `502 push_unconfirmed` when the publish fails:

```json
{
  "status": "ready",
  "redis": {
    "reachable": false,
    "probed": true,
    "error": "dial tcp ...: connect: connection refused",
    "degrades": [
      "all activity events, including to clients on this instance",
      "watch notifications",
      "session presence and session-targeted push"
    ]
  }
}
```

The `redis` block is absent entirely when no Redis is configured — "not
applicable" rather than "down".

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
- [ ] **Stream-honesty alerting:** `pad_event_resume_gaps_total` alerting on a rate that does NOT settle after a deploy (a step around one is expected — cold replay buffers), and `pad_event_receive_loop_exits_total` read as a rate against a stable subscriber count. See the metrics table above for what each label means
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
