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
| `PAD_WATCH_HEARTBEAT` | `false` | Phase 2 of half-open detection on the **watch** stream (`/api/v1/events/stream`). Independent of `PAD_EVENTS_HEARTBEAT` — the two buses hold different connections with different fates — and rolled the same way, phase 1 everywhere first. Ignored without Redis. |
| `PAD_EVENTS_HEARTBEAT` | `false` | Phase 2 of the half-open-connection detection rollout: publish a bus-internal liveness frame on each subscribed workspace channel every 30s. **Only set this once every instance runs a binary that recognises it** — see *Half-open connection detection* below. Setting it early makes every un-upgraded instance resync all its clients every 30 seconds. Ignored without Redis. |

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
>    `sync_required` (see *What `sync_required` means to a client*), by way of
>    its cold replay-buffer coverage check rather
>    than an epoch comparison — a freshly namespaced bus has no old epoch to
>    compare against. Expect a burst of client reconciliation as they reconnect
>    — for ACTIVITY-stream clients (the web UI) an incremental `/changes` delta
>    each, not a full page load. WATCH-stream clients cost less: `pad watch
>    --stream` answers `sync_required` by clearing its cursor and keeping the
>    connection open, so it refetches nothing. Either way that is the cutover
>    being paid for, and it is bounded by the number of
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

#### What `sync_required` means to a client

Both SSE endpoints — `/api/v1/events` (activity, workspace-scoped) and
`/api/v1/events/stream` (watch, user-scoped) — emit a `sync_required` event
when the server cannot honestly claim the client has seen everything. The
client's answer is to reconcile: the web client runs an incremental `/changes`
delta (not a full page load), and the `pad` CLI clears its cursor so its next
reconnect starts fresh.

**It is emitted in two situations, not one.** The distinction matters for
reading the metrics below, and for anyone writing a third-party consumer:

- **On a resume.** The client reconnected with a `Last-Event-ID` this instance
  cannot vouch for — an evicted or cold replay buffer, coverage that starts
  above the cursor, an ID-space change, or a cursor it cannot parse.
- **Mid-stream, on a connection that is still open.** The instance discovered
  it under-delivered to a client that never disconnected. Two causes: that one
  connection was too slow to drain its buffer, so an event was dropped for it;
  or this instance itself missed messages from Redis, which every subscriber on
  it shares.

  **Both streams now detect a pub/sub resubscription and a message they could
  not decode** (BUG-2739), and end the affected coverage when they do. Before
  this the watch stream detected neither, and for a client HOLDING A STREAM
  OPEN its only signal was a hole in the received ID sequence — which needs a
  LATER notification to expose it, so a flap that lost the newest notification
  on a stream that then went quiet left a connected CLI silently stale
  indefinitely. Detecting the two conditions directly is what covers the case
  ID arithmetic never reaches.

  A RECONNECTING client was never in that position and still is not: a resume
  asks the shared counter what the newest ID is rather than trusting the
  instance's local view, so a cursor the instance cannot vouch for is refused
  whether or not the instance ever noticed the flap. The gap this closes is
  specifically the open-stream one.

  **ID-sequence detection itself is watch-stream only, and that asymmetry is
  by construction rather than an omission.** The watch stream has one channel
  and one counter, so its IDs are consecutive and a hole is visible as a jump.
  The activity stream's IDs come from a counter shared across workspaces, so
  per-workspace holes are the NORMAL state and no arithmetic on them means
  anything. That is why `pad_watchevents_sequence_gaps_total` has no
  `pad_event_*` counterpart.

  **What a failover now COSTS, since detecting it is not free.** A watch-bus
  resubscription ends coverage for that instance's whole watch stream — there
  is one replay buffer, not one per workspace — so every `/api/v1/events/stream`
  subscriber on that instance is told mid-stream at once. Activity-stream
  coverage is per-workspace, so a resubscription there ends only the affected
  workspace's.

  What that costs depends entirely on the client, and for the one client that
  uses the watch stream today it is nearly nothing: `pad watch --stream` reacts
  to `sync_required` by clearing its cursor and KEEPING THE CONNECTION OPEN
  (`cmd/pad/cmd_watch.go`), so a failover produces no reconnect and no refetch
  — the next notification simply starts a fresh coverage span. The cost to
  watch out for is a future consumer that answers `sync_required` with a
  refetch instead: for that client the announcement is one request per
  connection, arriving together, since per-connection coalescing smooths
  repeats WITHIN a wave and not the wave itself. That is the deliberate trade
  this family makes — chatty-but-correct beats quiet-but-lossy — and if it
  ever becomes a capacity problem the answer is fewer connections per
  instance, not a quieter bus.

  **What `undecodable_message` actually indicates.** Genuinely unreadable
  input on the watch channel: a non-Pad publisher on the key, a wire format
  from a mixed-version fleet mid-upgrade, or corruption. **It does NOT
  usually mean two current Pad installations sharing a Redis** — those publish
  the same wire format, so their messages DECODE, and the damage is
  cross-feeding real notifications between installations while this counter
  stays flat. That is the failure `PAD_REDIS_NAMESPACE` exists to prevent, and
  it is both worse and quieter than the one this counter reports.

  **Who can force a resync with it, and what a flood costs.** Anyone who can
  `PUBLISH` onto the watch channel — which sounds worse than it is, since the
  same access allows publishing FORGED notifications, so a channel writer is
  outside the threat model already. Under a flood, what IS bounded: the
  announcement, a non-blocking send onto a capacity-1 flag that is already
  raised, so it collapses to nothing after the first; and heap GROWTH, since
  each discarded replay buffer is garbage immediately and the receive loop is
  serial. What is NOT bounded: per-message CPU and allocation — a fresh replay
  buffer plus a pass over every subscriber, per malformed message, on the
  single goroutine that also delivers real notifications, so a sustained flood
  is receive-loop starvation as much as it is garbage collection. And log
  volume, one ERROR line per message. Bounding either needs a rate threshold,
  which is a deployment decision this code declines to make on your behalf.
  Payload size is deliberately not capped in Pad, because go-redis has read the
  whole message into memory before Pad sees it; bound it with Redis's
  `proto-max-bulk-len` and with who holds `PUBLISH`.

  **One gap in that detection remains everywhere, and a second remains on the
  watch stream only.** A message lost in transit with the connection intact —
  no flap, no decode failure, just a message that never arrived (BUG-2735): on
  the watch stream a LATER notification exposes it as an ID gap, while on the
  activity stream, whose per-workspace IDs are non-consecutive by construction,
  nothing local ever does. That one is open on both.

  A HALF-OPEN connection — a route that stopped carrying traffic without
  closing, so nothing ever resubscribes and no message ever arrives to be
  non-consecutive with — is **closed on both streams**: on the activity stream
  by BUG-2738 and on the watch stream by BUG-2769. Do not assume go-redis's
  pub/sub health check covers it on either: `PubSub.Ping` writes the command and
  never reads a reply, so it reports healthy for as long as the socket accepts
  writes. What closes it is application-level idle tracking with a heartbeat
  that makes the threshold answerable — see *Half-open connection detection*,
  which covers both buses and their two independent phase flags.

  **A third residual affects RESUMES rather than open streams** (BUG-2743): if
  the watch counter restarts without the epoch rotating — evicted under
  `maxmemory`, lost to a `FLUSHDB`, restored from a stale snapshot — the old
  and new ID spaces overlap, and a `Last-Event-ID` inside that overlap cannot
  be attributed to either. The instance refuses the cursors it can identify as
  stale and serves the rest, so a client holding an old-space cursor in the
  overlap can be handed new-space notifications as though they followed it.
  Arithmetic on the IDs cannot close this — telling two sequences apart is
  what the epoch token is for, and this is precisely the case the epoch does
  not see. Rotating the epoch (see *Event ID-space migration*) is what makes a
  deliberate counter reset safe.

  A RECONNECTING client is largely covered on the watch stream anyway, because
  a resume consults the shared counter rather than local state alone. Not
  entirely: that check reads the counter at one instant, so a notification
  published AFTER the read and missed is invisible to it — an at-most-once
  pub/sub residual with no per-connection ack, documented on
  `resumeOutrunsLocalView` and again in the CLI. What these two gaps reliably
  leave stale is the client holding a stream OPEN.

The second case is newer — before it, a held-open stream that missed events was
never told, and a later delivered event advanced its cursor past the missing
IDs so no replica would ever replay them. A mid-stream `sync_required` carries
an empty `id:` field, exactly as the resume case does, so a client stops
resending a position the server has just disclaimed.

There is no separate event name for the mid-stream case, deliberately: every
client acts on the two identically.

**What a client should DO with it**, since the two endpoints recover
differently and a third-party consumer cannot infer this from the frame:

- **Keep the connection open.** The frame is not a close and does not ask for a
  reconnect. The server keeps streaming; a client that tears down and redials
  on every `sync_required` turns one delta into a reconnect storm.
- **Expect events after it, possibly with IDs below the hole.** A mid-stream
  `sync_required` is not ordered against events the server had already queued
  for that connection, so a client can receive the frame and then events that
  predate the gap. Their IDs re-establish a cursor at a position the server has
  just disclaimed. This is deliberate and bounded: reconciling is what the
  frame asked for, and a later reconnect from such a cursor is refused by the
  coverage check and answered with `sync_required` again. Holding the
  announcement back until those events drained was tried and removed — every
  version of it could defer the announcement indefinitely while a busy
  workspace kept the queue full, and an unbounded silence is worse than a
  redundant resync.
- **Stop trusting your cursor.** The empty `id:` retires it, so a compliant SSE
  client stops sending `Last-Event-ID` on its next reconnect. Do not re-send
  the old value: the server has just said it cannot vouch for that position.
- **On `/api/v1/events`, reconcile the workspace.** Its events describe item
  state, so a delta refetch recovers everything missed. The web client uses
  `/changes`; any client can re-read the items it cares about.
- **On `/api/v1/events/stream`, reconcile what you can and accept the rest is
  gone.** Watch-matched notifications describe item state and can be re-derived
  by re-reading those items. One-shot PUSHES cannot: they are not stored as
  recoverable state, there is no backfill endpoint for them, and a push missed
  during a hole is missed permanently. This endpoint is best-effort for pushes
  by design, and `sync_required` on it means "your position is untrustworthy",
  not "re-fetch and you will be whole again". The `pad` CLI monitor does
  exactly this: it clears its cursor and keeps listening. There IS a separate metric — see
`pad_event_midstream_resyncs_total` below — so the two populations stay
distinguishable to an operator without changing what any existing alert means.

**A connection gets at most one MID-STREAM announcement every 5 seconds** (the
resume-time signal is not rate-limited and never needed to be — it happens once
per connection, at the start), and nothing is lost to that bound: a gap
arriving inside the window is remembered and announced when the window closes. The bound exists because the subscriber most likely to be
signalled is a slow one, and answering "you could not keep up" with "now fetch
a delta" can feed back into more drops.

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
| `pad_watchevents_resume_gaps_total` | Resumes this instance could not serve — from a hole, a cold start, an epoch change, or a shared-counter disagreement. Each sends a client `sync_required`. RESUME-TIME ONLY; a subscriber told mid-stream is counted separately, so an alert on this keeps the meaning it had |
| `pad_watchevents_midstream_resyncs_total` | Watch-stream subscribers told MID-STREAM that they missed notifications, on a connection that stayed open. New in BUG-2730 |
| `pad_watchevents_notifications_missed_total` | How many notifications those gaps spanned |
| `pad_watchevents_notifications_dropped_total` | Received but not delivered to a local subscriber — that connection's buffer was full. Since BUG-2730 that subscriber is told (`sync_required`, mid-stream) rather than silently under-served, so a rise here produces a rise in `pad_watchevents_midstream_resyncs_total`, one client at a time |
| `pad_watchevents_sequence_resets_total` | Watch replay coverage dropped, by reason. `epoch_change` — the watch epoch token changed, so the IDs now come from a different sequence; the token is an opaque UUID here, not a numeric generation. `counter_backward` — an ID arrived at or below the high-water mark with the epoch unchanged. (This label was spelled `counter_backwards` while BUG-2739 was in development. If you are reading a dashboard that uses the plural, it was built against an unreleased build — see the note below.) `subscription_resumed` — a pub/sub connection dropped and re-subscribed, so whatever was published during the outage never arrived; expect these during a Redis failover and expect them to stop afterwards. `undecodable_message` — a message on the watch channel could not be parsed. The instance cannot tell whether that was a notification it should have had or something foreign, and it stops vouching because it cannot tell; expect zero, and suspect a namespace collision. `idle_timeout` — the subscription received nothing at all (no notification, no heartbeat, no subscription confirmation) for longer than the idle timeout, so this instance stopped vouching for its buffer and replaced the connection; it means the socket stopped PROVING it works, not that notifications were observed going missing, and it is structurally never emitted on watch-heartbeat phase 1. Unlike the activity stream's twin it needs no companion counter, because `dropCoverage` here replaces the buffer and reports unconditionally rather than only when one existed. The first two mean the ID space changed under this instance. `subscription_resumed` means it did not and something demonstrably went missing. `undecodable_message` means neither is established — only that coverage can no longer be proved. Each also announces to the watch subscribers connected at that moment, so each moves `pad_watchevents_midstream_resyncs_total` by AT MOST one per such subscriber — at most, because the signal is capacity-1 and coalescing, so a second cause firing before a client has acted on the first adds no announcement. For the same reason the announcement counter is not a ratio against this one in aggregate: it also counts gaps and slow-subscriber drops, and only a reset observed in isolation, against idle clients, lets you read the fan-out off these two counters |
| `pad_watchevents_receive_loop_exits_total` | Non-zero outside shutdown means an instance publishes but receives nothing |
| `pad_event_resume_gaps_total` | The ACTIVITY stream's (`/api/v1/events`) twin of the watch resume counter above. **Expect a step around a deploy, with the RATE settling back to baseline** (the counter itself only ever increases) — each instance starts with no replay coverage, so an early resume against a workspace it has not seen yet is a warranted resync. It counts RESUMES, not clients: a deploy with no reconnects does not move it at all, and a client that reconnects several times is counted several times. A rate that does not settle is the thing to alert on |
| `pad_event_midstream_resyncs_total` | Activity-stream subscribers told MID-STREAM that they missed events, on a connection that stayed open. New in BUG-2730, and the counter to watch when judging whether that fix is costing more resyncs than it is worth. It counts ANNOUNCEMENTS, not causes and not distinct clients: a reset that drops buffers moves it once per live subscriber (and that ratio against `pad_event_sequence_resets_total` is the fan-out); a burst of drops on ONE connection moves it once, because signals coalesce and are rate-limited per connection; and a coverage loss on a workspace with no buffer yet moves it while every cause counter stays flat, because there was no coverage to end but the subscribers still have a hole |
| `pad_watchevents_midstream_resyncs_total` (see also, listed above) | Same meaning for the watch stream. Its causes are a slow-subscriber drop and a received sequence gap or reset; a gap announces to EVERY subscriber on the instance, so it can exceed all of its cause counters |
| `pad_event_sequence_resets_total` | Activity replay coverage dropped, by reason. `subscription_resumed` — a pub/sub connection dropped and resubscribed, dropping that workspace's buffer; expect it during a Redis failover and expect it to stop afterwards. `epoch_change` — the shared counter's ID space changed generation, dropping every buffer; expect a handful per cutover. `counter_backward` — an ID arrived at or below a buffer's high-water mark with no generation change; see *Event ID-space migration* for what to expect per phase. `epoch_regressed` — a LOWER generation was seen, so this instance stopped vouching for its buffers. One alongside an `epoch_change` is a message that was in flight when the generation rotated; a RUN of them means the counter itself went backwards — usually Redis lost writes, and since BUG-2740 possibly a repaired generation key (see *A repaired generation counter*). `undecodable_message` — a message on these channels could not be parsed, so that workspace's coverage ended; expect zero, and suspect a namespace collision. `subscription_unconfirmed` — a subscription was admitted before Redis acknowledged the SUBSCRIBE and the acknowledgement then arrived, so the span in between is one that stream cannot account for; it reaches THIS counter only when a buffer existed to drop, so read `pad_event_subscription_unconfirmed_total` for the dependable count. `idle_timeout` — a subscription received nothing at all (no event, no heartbeat, no acknowledgement) for longer than the idle timeout, so this instance stopped vouching for its buffer. It means **coverage ended, not that the connection was replaced**: the replacement is attempted afterwards and installs nothing if the instance is shutting down or the workspace loses its last subscriber, so only `pad_event_subscription_cycled_total` proves a replacement. Unlike `subscription_resumed` it does NOT establish that events went missing, only that the socket stopped proving it works, and like `subscription_unconfirmed` it reaches this counter only when a buffer existed to drop |
| `pad_event_events_dropped_total` | Activity events not delivered to a live subscriber, by reason — today only `slow_subscriber` (that connection's 64-deep channel was full). Per-SUBSCRIBER: every subscriber that was keeping up received the event. Pairs with `pad_event_midstream_resyncs_total`, though not one-for-one in either direction — see that row. New in BUG-2730, along with the fix that stops the drop being silent, so a deploy that starts reporting these is not necessarily a regression — it may be the first time they were countable |
| `pad_event_subscription_cycled_total` | Activity-stream workspace subscriptions torn down **and replaced** because nothing arrived on them — no event, no heartbeat, no acknowledgement — within the idle timeout. It counts replacements, not teardowns: a cycle that installed nothing because the instance was shutting down or the workspace lost its last subscriber does not increment it, so a restart cannot manufacture this signal. Detects a **half-open connection**: no FIN, no RST, just a route that stopped working, which go-redis cannot see because its pub/sub health check writes a PING and never reads the reply. **Expect zero.** Read this rather than `pad_event_sequence_resets_total{reason="idle_timeout"}`, which moves only when a buffer existed to drop and so under-reports exactly the early-wedge case this detector exists for. A non-zero rate means connections to Redis are being silently blackholed — a NAT idle timeout, a stateful firewall, an overlay network dropping long-lived flows; check TCP keepalive on the path before changing the interval. **On heartbeat phase 1 this counter is structurally zero** — detection is part of phase 2, so a zero there says nothing at all about whether any route has wedged. Read `heartbeat_phase` off the startup log before drawing any conclusion from it |
| `pad_event_subscription_unconfirmed_total` | Activity-stream subscriptions admitted before Redis acknowledged the SUBSCRIBE, because the wait for it timed out (BUG-2747). **Expect zero.** Counts ESTABLISHMENTS, not clients — one workspace subscription that timed out increments it once however many subscribers were waiting on it. Nothing is known to have been lost; what it says is that a stream was admitted whose coverage this instance cannot describe, and that every subscriber waiting on it will be told to reconcile when the acknowledgement lands. A non-zero rate means the SUBSCRIBE round trip is slow or stalling — read it alongside SSE connect latency rather than alongside `pad_event_sequence_resets_total` |
| `pad_event_receive_loop_exits_total` | A workspace's activity subscription loop stopped. Unlike the watch stream's twin this does **not** stay at zero — it is expected at shutdown and whenever a workspace's last local subscriber leaves. Read it as a rate against a stable subscriber count |
| `pad_session_presence_failures_total` | Presence operations failing — **read the `op` label**, the risks differ and run in opposite directions: `register`/`renew` may under-report (a live session unlisted and untargetable), `deregister` may over-report (a dead session left listed, and a push aimed at it reaches nobody), `list` returns a 503, `prune` is benign. A failure means the operation reported an error — Redis can fail a pipeline after applying it, so the write may have landed anyway |


#### A repaired generation counter

`event_epoch_gen` is a shared Redis key, and the same things that corrupt any
shared key can corrupt it: a namespace collision with another installation, a
hand-edit during an incident, a restore that mixed keyspaces. Since BUG-2740 a
corrupted one is REPAIRED rather than fatal — before that, every phase-2
publish consumed a sequence ID and then failed, permanently, because the branch
that would have rotated the generation was the branch that could not run.

Two operator-visible consequences, neither of which had documentation:

- **A repair reseeds the generation from wall-clock SECONDS.** That is above
  any counted history, so it normally reads as an ordinary `epoch_change`. It
  is not guaranteed to be above a counter that a collision or a hand-edit had
  pushed higher, so it can instead surface as `epoch_regressed` — which
  otherwise means a failover to a replica that lost writes. **The tell is the
  value**: read the key, and a repaired generation looks like a unix timestamp
  (ten digits, around 1.7e9) rather than a small count of ID-space resets.
  There is no repair-specific counter or log line, because the repair happens
  inside a Lua script.
- **Clients reconcile, and normally once.** A repair is an ID-space change
  like any other, so receivers stop vouching for their buffers. It does not
  loop, because the repaired key is valid and the next rotation increments it
  normally. The exception is a repaired generation that lands BELOW the one a
  receiver already holds: that instance discards the lower epoch as a
  straggler for its 30-second window rather than adopting it, so the same
  space can be disclaimed again when it is finally adopted. Bounded by that
  window, and visible as `epoch_regressed` rather than `epoch_change`.

**Two repairs CAN collide, and what catches it is not the epoch.** The seed is
above any COUNTED history; it is not a monotonicity guarantee. Corrupt the key
twice inside one second and both repairs seed the same value, so two genuinely
different ID spaces carry the identical epoch — and an equal epoch means "same
space" by design, so neither `epoch_change` nor `epoch_regressed` fires.

The detection chain that does hold, stated so nobody has to rediscover it:

> A merge requires IDs to be REUSED at a receiver. Reuse requires the sequence
> counter to go BACKWARDS. A backwards counter is detected regardless of what
> the epoch says — it is the `counter_backward` reason, which drops the
> affected buffers and refuses cursors below the discarded high-water mark.

So the guarantee is carried by a different detector than the epoch mechanism
suggests. That is deliberate and it is tested end to end
(`TestACollidingRepairIsCaughtBySequenceRatherThanEpoch`), because a future
change that weakened `counter_backward` would remove a protection nothing else
here advertises.

Two cases that look similar and are not. A sequence counter set FORWARD — say
to 50, so the next ID is 51 — is a jump inside ONE space: IDs stay unique and
increasing, nothing is reused, and per-workspace IDs are non-consecutive by
construction anyway. And a receiver that never held the colliding range has
nothing to merge; what it experiences is a gap, which is the pre-existing
undetectable-loss case tracked as BUG-2735.


**`pad_watchevents_sequence_resets_total` has no released contract yet, and
that is why BUG-2739 could change it freely.** The whole metric was introduced
after `v0.14.0` and no tagged release emits it, so nothing outside a
development deployment can be alerting on it. Two things about it changed on
that branch: the `counter_backward` label lost a trailing `s`, and the metric
widened from "the ID space changed" to "replay coverage was dropped", which
added the `subscription_resumed` and `undecodable_message` reasons. A
reason-specific alert on `epoch_change` is unaffected; one on
`counter_backward` must have its expression updated for the spelling, which is
the whole reason this paragraph exists. An alert on the unlabelled total now
counts more things, which is the metric doing what its name says rather than a
regression. During a rolling deploy an instance on
the older build reports neither new reason and keeps the old spelling — so a
mixed fleet reports two shapes under one name for the rollout's length, which
is acceptable precisely because no released version is in that fleet.

**Re-derive that rather than trusting this paragraph**, because it is a claim
about release state and release state changes without anyone editing this file:

```
git describe --tags --abbrev=0 origin/main      # the latest tag
git log --reverse --format=%H -S pad_watchevents_sequence_resets_total \
  -- internal/metrics/metrics.go | head -1      # the commit that introduced it
git merge-base --is-ancestor <commit> <tag>     # non-zero exit => still unreleased
```

Once a release does ship this metric, the next change to it is a real contract
break and needs versioned treatment instead of a note here.


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

The fix gives each ID space an **epoch** — a generation number (monotonic in
normal operation; see *A repaired generation counter* below for the one case
that is not),
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

**One narrow window during the phase-2 roll.** Once a replica has adopted the
epoch, a message from an un-flipped instance carries no epoch and is treated as
belonging to the current space — which it does, unless the sequence counter
reset between that publisher assigning its ID and publishing it. An ID from the
dead space can then land in a buffer describing the new one. There is no way to
tell the two apart from the message alone, and the alternatives are worse: a
replica that refused un-flipped messages would resync its clients on every one
of them for the length of the roll. It usually ends loudly and quickly: the next
event that workspace receives is lower than the straggler's ID, which trips
`counter_backward`, drops the buffers and is reported. It is not guaranteed to
— the sequence counter is shared across workspaces while that check is per
workspace, so if other workspaces carry the counter past the straggler's value
first, nothing fires and the dead-space ID stays in that workspace's buffer.
Closing that needs the same thing the residual below needs.

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

#### Half-open connection detection (`PAD_EVENTS_HEARTBEAT`, `PAD_WATCH_HEARTBEAT`)

**Two buses, two flags, rolled independently.** The activity stream
(`/api/v1/events`) and the watch stream (`/api/v1/events/stream`) hold different
Redis subscriptions with different fates, so each has its own phase-2 flag and
you can roll one before the other. Everything below applies to both; the
differences are collected at the end.

**The problem this fixes.** A TCP connection can stop carrying traffic without
closing — no FIN, no RST, just a route that stopped working. A NAT table
expiring, a stateful firewall dropping an idle flow, an overlay network
silently rerouting. The instance behind it blocks on a read that will never
return, receives nothing, and its replay buffer goes on looking complete. Every
resume for that workspace is then answered "caught up" from a coverage window
that ended when the route did — silent loss, with nothing in any metric.

**Why go-redis does not cover it.** Its pub/sub health check writes a `PING`
and never reads a reply, so its error stays nil for as long as the socket
accepts writes — which a half-open socket does until its send buffer fills. The
channel path sets no read deadline either. Measured, not assumed: against a TCP
proxy that silently stopped forwarding, with the health check running, there
was no reconnect in 24 seconds.

**What the fix does.** Every subscription records when it last received
anything — an event or notification, a subscription acknowledgement, or a
heartbeat. When that goes stale past the idle timeout, the instance ends that
subscription's replay coverage (a workspace's on the activity stream, the
instance's on the watch stream — so the next resume answers `sync_required`
rather than "caught up")
**and replaces the connection**. Dropping coverage alone would not recover: the
resync it demands is served from the same dead socket, and the detector fires
again on the next pass — a loop metering the failure rather than fixing it.

**Why a heartbeat, rather than just a threshold on real traffic.** "Is this
stream quiet, or is the route dead?" cannot be answered from traffic — it
depends on your publish rate, and no constant is right for every deployment.
Publishing our own frame replaces it with "did our heartbeat arrive?", which is
answerable everywhere. The instance publishes one frame every **30 seconds**
(T) — per subscribed workspace on the activity stream, once per instance on the
watch stream, and cycles a subscription that has received nothing for
**90 seconds** (3T). Three intervals rather than two so a single lost or late
frame is not a cycle. Detection latency measured from the last frame that got
through is 90–120s — the scan runs on its own 30s cadence, which adds up to one
interval on top of the threshold. Measured from the moment the route actually
died it is wider, roughly 60–120s: the publisher runs on an independent
schedule, so the last frame through may have been sent anywhere in the interval
before the fault.

**Detection is part of phase 2, not phase 1.** Publishing and detecting are one
capability with one switch, because an instance detects off its *own* frames —
it publishes to the workspace channels it subscribes to and receives them back,
so it never depends on peers having flipped. A phase-1 instance therefore
detects nothing; it only recognises the frame so that a phase-2 peer costs it
nothing. Splitting them was tried and is wrong: with no heartbeat and no
events, a perfectly healthy *quiet* workspace crosses the threshold every
90–120s and gets cycled, which is a resync storm on the default configuration
every deployment lands in first.

**It rolls out in two phases, and the order is not optional.**

| Phase | What you do | What instances publish | What they do with a frame |
|-------|-------------|------------------------|---------------------------|
| 1 | Roll the new binary everywhere. Leave both flags unset. | No heartbeats | Recognise and ignore it. **No idle detection.** |
| 2 | Set the flag (`PAD_EVENTS_HEARTBEAT` and/or `PAD_WATCH_HEARTBEAT`) and roll again. | One frame per 30s — per subscribed workspace on the activity bus, once per instance on the watch bus | Recognise and ignore it. **Idle detection active.** |

**What happens if you run them out of order.** The frame has to travel on the
same channel the stream's own traffic does — the workspace's *event* channel on
the activity bus, the single watch channel on the watch bus — because that
channel's connection is the thing whose liveness is in question; a probe
anywhere else proves the wrong thing. An instance running a **pre-phase-1**
binary cannot classify it: the frame falls through to that bus's decoder, fails
to parse, and is treated as a hole in coverage. That instance drops the replay
buffer **and tells every one of its live subscribers to resync** — every 30
seconds, for as long as the deployment is mixed. On the activity bus that is per
workspace, so the noise scales with how many an instance is subscribed to; on
the watch bus it is one buffer and one announcement round per instance. Either
way the blast radius is the instances you have *not* upgraded, which no amount
of care in the new code can reach. This is noisier than the ID-space migration's
equivalent mistake and it is the reason both defaults are off.

Both rolls are zero-loss in the other direction: phase-1 instances recognise the
frame from the release that introduces it, so during the phase-2 roll a mix of
publishing and non-publishing instances is exactly the case ignore-the-frame
exists for.

**Rolling back to phase 1** is safe and takes effect immediately: make the
effective value **false** and roll. Peers ignore the frame throughout, and idle
detection stops with it — you are back to the pre-BUG-2738 behaviour, which is
a wedged route going unnoticed, not a worse one. The same two wrinkles as
the ID-space migration apply, for the same reasons:

- **Setting the value to false is not the same as unsetting the environment
  variable.** `events_heartbeat` can also be set in `~/.pad/config.toml`, and
  the file's value stands when the environment variable is absent. Clear both,
  or set the environment variable explicitly to `false`.
- **Downgrading past phase 1 is a SECOND step, in the reverse order.** A
  pre-phase-1 binary still cannot classify the frame. Roll every instance to
  phase 1 (new binary, flip off), let it finish, *then* downgrade the binary.

**The frame is validated, not just prefix-matched.** A liveness frame is
`hb|<version>` plus optional short tokens, under a length cap. Anything else
that happens to begin with `hb|` is treated exactly as any other unreadable
payload: that workspace's coverage ends and
`pad_event_sequence_resets_total{reason="undecodable_message"}` moves, which is
the signal that says *suspect a namespace collision*. A forged frame cannot
fake liveness in any case — liveness means "this socket carried traffic", and a
frame that arrives demonstrates that whoever sent it.

There is no Redis or database migration in either direction, and the frames are
never persisted: a heartbeat consumes no event ID, carries no epoch, is never
buffered or replayed, never reaches a subscriber, and is never counted as an
event. That last part is load-bearing rather than tidy — three of this bus's
reset reasons (`counter_backward`, `epoch_change`, `epoch_regressed`) are
derived from the shared ID counter, so a probe that consumed IDs would
manufacture the resets it exists to avoid.

**Which phase an instance publishes in is in its startup log**, as
`heartbeat_phase=1` or `heartbeat_phase=2` on the "Event bus using Redis pub/sub"
line, alongside `id_space_phase`. The two migrations are independent — any
combination is valid. An unparseable `PAD_EVENTS_HEARTBEAT` is ignored and logs
a warning naming the value.

**What this covers, and what it does not.** It is a *receive-side* detector,
not a round-trip health check. It measures whether frames arrive on a
workspace's subscription, so:

- A subscription whose *outbound* direction is broken but which still receives
  looks healthy — correctly, since nothing is being lost.
- The **PUBLISH path is not covered and cannot be.** `PUBLISH` travels on the
  client's ordinary connection pool while a subscription holds a connection
  from a separate pub/sub pool; those are different sockets with different
  fates, and a reconnect of one repairs nothing about the other. An instance
  whose publish path is wedged loses its own events for every other instance,
  and this feature will not tell you.
- **The replacement is attempted, not guaranteed.** If the path is still
  blackholed when the cycle re-dials, the new connection cannot receive either
  and the detector fires again on the next pass. Coverage stays ended
  throughout, so nothing is ever falsely claimed — but delivery resuming is a
  statement about your network, not about Pad. One case where the replacement
  can fail on a *healthy* path is tracked as BUG-2764: go-redis discards the
  error from the initial `SUBSCRIBE`, so a failed subscribe yields a connection
  that looks live and is subscribed to nothing. The detector cycles it again on
  the next pass, which is why this self-heals on phase 2 and does not on phase 1.

**What to watch.** `pad_event_subscription_cycled_total` — expect zero. Read it
rather than the `idle_timeout` reset label, which only moves when there was a
buffer to drop and therefore misses the early-wedge case. A non-zero rate is a
network fact about the path between your instances and Redis, not a Pad
condition: compare it against TCP keepalive settings on that path before
changing the interval, because a shorter interval treats the symptom and a
longer one widens the window the detector exists to bound.

**A residual an operator should know about, not fixed here.** When many
workspaces are cycled at once — a NAT table flush, a firewall rule change, an
overlay network dropping every long-lived flow — every connected subscriber of
every affected workspace is told to resync in the same instant. The SSE
connections stay open, so this is *not* a reconnect storm and the admission
limits are not involved; what it produces is a burst of `/changes` requests
against the database, coalesced per browser tab but with no jitter and no
global budget. This is not new with the heartbeat: a Redis failover already
signals every workspace at once through `subscription_resumed`. What is new is
a second trigger of the same class. Tracked separately; if you run a large
fleet, watch database load alongside
`pad_event_sequence_resets_total` after any network event that could wedge many
routes simultaneously. Tracked as BUG-2761.

**Cost on the activity stream.** Each workspace has its own Redis subscription —
and therefore its own connection — so liveness is genuinely per-workspace and
there is no cheaper shared probe. An instance subscribed to N workspaces
publishes N frames every 30s; at N=1000 that is roughly 33 publishes/sec, which
is noise for Redis. If fleet workspace counts ever make it matter, the fix is
connection consolidation, not a longer interval.

**How the watch stream differs.** It holds ONE process-wide subscription on one
channel rather than one per workspace, which changes three things and nothing
else:

- **Cost is flat.** One frame per instance per interval regardless of how many
  workspaces or clients exist, against the activity bus's one per subscribed
  workspace.
- **A cycle affects every watch subscriber on the instance**, not one
  workspace's — but that is the path `dropCoverage` already takes for a
  resubscription or an undecodable message, so it is a third trigger on an
  existing announcement rather than a wider one. It is bounded: at most one
  announcement per connected subscriber per cycle (the signal is capacity-1 and
  coalescing), and at most one cycle per idle timeout per instance.
- **No separate cycle counter.** On the activity stream
  `pad_event_subscription_cycled_total` exists because its reset reason only
  fires when a buffer existed to drop. The watch bus drops its buffer
  unconditionally, so `pad_watchevents_sequence_resets_total{reason="idle_timeout"}`
  is already a complete count and a second metric would be noise.

`pad_watchevents_heartbeat_publish_failures_total` is the watch twin of the
activity stream's probe-failure counter and reads the same way: detection
degraded, not a peer broken.

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
