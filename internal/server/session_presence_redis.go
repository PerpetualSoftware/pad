package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RedisSessionPresence is the shared-state SessionPresence (BUG-2698) —
// the implementation session_presence.go's interface has been waiting for
// since PLAN-2558 S1, and the one that makes a multi-instance deployment
// tell the truth about who is listening.
//
// WHAT IT FIXES, precisely, because the obvious reading is too broad.
// BUG-2651 gave watchevents a Redis bus, so a notification PUBLISHED on
// any instance now reaches a stream held on any other. What stayed broken
// is everything that depends on knowing WHICH sessions exist:
//
//   - A session-targeted push was resolved against the presence registry
//     of whichever instance handled the POST, and handlers_push.go SKIPS
//     the publish entirely when the target is absent from that snapshot.
//     With per-process presence, a session connected to B was invisible to
//     A, so a POST that the load balancer sent to A dropped the
//     instruction and answered 200 delivered_sessions:0. Reachable through
//     the intended flow: GET /api/v1/sessions answered by B lists the
//     session, and the POST that follows is free to land on A.
//   - delivered_sessions was wrong in BOTH directions for a broadcast
//     push — a local count describing a global delivery.
//   - GET /api/v1/sessions under-reported, so the web push dialog's target
//     picker could not offer a session on another instance at all.
//
// One shared registry closes all three, and the ORDER matters: making the
// registry global makes the snapshot right, which makes the picker
// complete AND restores the push gate's original premise — at which point
// the existing skip is correct for the reason it was written, rather than
// being something to work around. Publishing unconditionally for targeted
// pushes would have fixed delivery and immediately made
// delivered_sessions:0 a lie in the other direction.
//
// THE REAPING STORY, which session_presence.go's interface doc requires of
// any out-of-process implementation and does not let this one inherit.
// MemorySessionPresence's entries die with the process that wrote them —
// there is nothing to reap, and Server.Shutdown leaving SSE handlers (and
// their presence entries) hanging is harmless for it. Redis entries
// OUTLIVE their writer, so a crash or a hard kill would otherwise strand
// them permanently, and the list would name sessions belonging to an
// instance that no longer exists.
//
// This implementation reaps with Redis's own expiry rather than with
// bookkeeping of its own:
//
//   - Each session is a key with a TTL (sessionKeyTTL), renewed every
//     sessionRenewInterval by a goroutine that lives exactly as long as
//     the connection's Add..Remove span. A process that dies stops
//     renewing, and Redis deletes the entries — no sweeper, no instance
//     ownership records, no startup scan.
//   - The per-user index is a SET carrying the same TTL. A member whose
//     session key has already expired is pruned by the next reader
//     (ListForUser), so the index self-heals without a background job.
//
// Native expiry is deliberate: the alternative — storing an expiry
// timestamp per entry and filtering at read time — compares a timestamp
// written by one instance's clock against another instance's clock, and
// "two instances disagree about presence" is the exact bug class this type
// exists to end.
//
// STALENESS IS UNCHANGED, and must not be read as improved. A session
// still disappears from this list only when its stream handler returns
// (clean disconnect) or the keepalive write fails (up to ~30s after an
// ungraceful one) — see LiveSession's doc comment. The TTL is a crash
// backstop, not a liveness probe: it is deliberately several renewal
// intervals long, so it never expires a session whose process is merely
// busy. Consumers must keep treating this list as "connected as far as the
// server can tell", never as a delivery guarantee.
type RedisSessionPresence struct {
	client *redis.Client

	// sessionKeyTTL and renewInterval are fields rather than constants so
	// tests can drive the expiry path without sleeping through it. Prefer
	// NewRedisSessionPresence, which sets the production pair.
	sessionKeyTTL time.Duration
	renewInterval time.Duration

	mu       sync.Mutex
	closed   bool
	renewals map[string]context.CancelFunc // userID|sessionID -> stop its renewal
	wg       sync.WaitGroup
}

const (
	// sessionRenewInterval matches watchEventsKeepaliveInterval (30s), the
	// cadence at which the stream handler already proves the connection is
	// alive. Renewing more often would buy nothing: a connection that has
	// died between keepalives is not detectable here either.
	sessionRenewInterval = 30 * time.Second

	// sessionKeyTTL is THREE renewal intervals, not one. A TTL close to the
	// interval would evict live sessions on any hiccup — a GC pause, a
	// briefly slow Redis, a rescheduled goroutine — and an evicted session
	// is invisible to the target picker AND causes handlers_push.go to skip
	// the publish for a session that is genuinely connected, which is
	// exactly the failure this type exists to remove. Three intervals means
	// a process must miss three consecutive renewals before its sessions
	// vanish, while a crashed instance's entries still clear inside 90s
	// rather than lingering forever.
	sessionKeyTTL = 3 * sessionRenewInterval
)

// sessionKey is the per-session entry: one key, one TTL, one owner.
func sessionKey(userID, sessionID string) string {
	return "pad:session:" + userID + ":" + sessionID
}

// sessionIndexKey is the per-user SET of that user's session ids. Scoped
// per user because every read is user-scoped — there is no "list all
// sessions on this server" consumer and there should not be one (see
// handleListSessions' doc comment).
func sessionIndexKey(userID string) string {
	return "pad:sessions:" + userID
}

// NewRedisSessionPresence returns a shared registry backed by client.
//
// The client is the SAME one the watch bus uses (cmd/pad/cmd_server.go
// dials once on PAD_REDIS_URL): they address the same server, and one
// connection pool serving two logical concerns is the shape internal/events
// already assumes.
func NewRedisSessionPresence(client *redis.Client) *RedisSessionPresence {
	return &RedisSessionPresence{
		client:        client,
		sessionKeyTTL: sessionKeyTTL,
		renewInterval: sessionRenewInterval,
		renewals:      make(map[string]context.CancelFunc),
	}
}

// Add implements SessionPresence.
//
// Returns the generated id even when the Redis write fails. That is
// deliberate and it is the same posture the rest of the push path takes
// toward presence: the caller is a live SSE handler that is about to serve
// a real connection, and it must not be torn down because an optional
// registry is unavailable. A session missing from the registry is
// under-reporting — the failure mode presence already documents — while
// refusing the connection would take away delivery the bus can still
// perform. The id is still the one watchNotificationVisible compares a
// targeted push against, so a broadcast push reaches this connection
// regardless.
func (p *RedisSessionPresence) Add(userID string, ident SessionIdentity) string {
	id := uuid.NewString()
	sess := LiveSession{
		ID:          id,
		Label:       ident.Label,
		PID:         ident.PID,
		Armed:       ident.Armed,
		ConnectedAt: time.Now().UTC(),
	}
	payload, err := json.Marshal(sess)
	if err != nil {
		// Cannot happen for this struct; logged rather than swallowed so a
		// future field that breaks marshalling is visible.
		slog.Error("session presence: marshal session", "error", err, "user_id", userID)
		return id
	}

	ctx, cancel := context.WithCancel(context.Background())

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		cancel()
		return id
	}
	p.renewals[renewalKey(userID, id)] = cancel
	p.wg.Add(1)
	p.mu.Unlock()

	if err := p.write(context.Background(), userID, id, string(payload)); err != nil {
		slog.Warn("session presence: failed to register session; it will be missing from the picker",
			"error", err, "user_id", userID)
	}

	go p.renewLoop(ctx, userID, id, string(payload))
	return id
}

// write stores the session entry and indexes it, both under the same TTL.
// Pipelined rather than scripted: the two commands are independent and a
// partial application is self-correcting — an index member with no session
// key is pruned by the next ListForUser, and a session key with no index
// member expires on its own.
func (p *RedisSessionPresence) write(ctx context.Context, userID, sessionID, payload string) error {
	pipe := p.client.Pipeline()
	pipe.Set(ctx, sessionKey(userID, sessionID), payload, p.sessionKeyTTL)
	pipe.SAdd(ctx, sessionIndexKey(userID), sessionID)
	pipe.Expire(ctx, sessionIndexKey(userID), p.sessionKeyTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// renewLoop keeps this connection's entry alive for exactly as long as the
// connection is held open by THIS process.
//
// It re-SETs the payload rather than issuing a bare EXPIRE, so a renewal
// also RESTORES an entry that expired during a Redis outage or a long
// pause. A bare EXPIRE against a vanished key is a no-op that returns
// success, which would leave a live session permanently invisible with
// nothing in the logs.
func (p *RedisSessionPresence) renewLoop(ctx context.Context, userID, sessionID, payload string) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.write(ctx, userID, sessionID, payload); err != nil && ctx.Err() == nil {
				slog.Warn("session presence: failed to renew session entry",
					"error", err, "user_id", userID)
			}
		}
	}
}

// Remove implements SessionPresence: idempotent, and safe for a session id
// that was never added (the stream handler's defer runs on paths where Add
// may not have been reached).
func (p *RedisSessionPresence) Remove(userID string, sessionID string) {
	if sessionID == "" {
		return
	}
	p.mu.Lock()
	if cancel, ok := p.renewals[renewalKey(userID, sessionID)]; ok {
		delete(p.renewals, renewalKey(userID, sessionID))
		cancel()
	}
	p.mu.Unlock()

	// A fresh context, NOT the cancelled renewal one: this is the clean
	// deregistration a disconnect owes the registry, and it has to run
	// after the renewal has stopped. Reusing the cancelled context would
	// fail every command and leave the entry to expire on its TTL instead —
	// turning an immediate disconnect into a 90-second ghost in every other
	// instance's session picker.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pipe := p.client.Pipeline()
	pipe.Del(ctx, sessionKey(userID, sessionID))
	pipe.SRem(ctx, sessionIndexKey(userID), sessionID)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("session presence: failed to deregister session; it will expire on its TTL",
			"error", err, "user_id", userID)
	}
}

// ListForUser implements SessionPresence, oldest connection first with the
// session id as a tiebreaker — the same deterministic order
// MemorySessionPresence produces, because an unstable order would make the
// web target picker jump around under the user's cursor.
//
// Returns what it can rather than failing: a Redis error yields an empty
// list, matching the nil-registry behaviour every consumer already handles.
// handleListSessions is the one caller that distinguishes "no sessions"
// from "couldn't read" — it 503s on a nil registry — and this method does
// not manufacture that distinction, because a partial read is not an
// outage.
func (p *RedisSessionPresence) ListForUser(userID string) []LiveSession {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ids, err := p.client.SMembers(ctx, sessionIndexKey(userID)).Result()
	if err != nil {
		slog.Warn("session presence: failed to read session index", "error", err, "user_id", userID)
		return nil
	}
	if len(ids) == 0 {
		return nil
	}

	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, sessionKey(userID, id))
	}
	values, err := p.client.MGet(ctx, keys...).Result()
	if err != nil {
		slog.Warn("session presence: failed to read session entries", "error", err, "user_id", userID)
		return nil
	}

	out := make([]LiveSession, 0, len(values))
	var expired []string
	for i, v := range values {
		if v == nil {
			// The session key's TTL lapsed — its process stopped renewing,
			// i.e. it died without deregistering. Prune the index member so
			// a crashed instance's leftovers don't accumulate.
			expired = append(expired, ids[i])
			continue
		}
		raw, ok := v.(string)
		if !ok {
			continue
		}
		var sess LiveSession
		if err := json.Unmarshal([]byte(raw), &sess); err != nil {
			slog.Warn("session presence: undecodable session entry, skipping",
				"error", err, "user_id", userID)
			continue
		}
		out = append(out, sess)
	}
	if len(expired) > 0 {
		// Opportunistic and best-effort: a failure here costs nothing but a
		// retry on the next read, and the members are already invisible to
		// this listing either way.
		if err := p.client.SRem(ctx, sessionIndexKey(userID), toAny(expired)...).Err(); err != nil {
			slog.Debug("session presence: failed to prune expired index members",
				"error", err, "user_id", userID)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].ConnectedAt.Equal(out[j].ConnectedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].ConnectedAt.Before(out[j].ConnectedAt)
	})
	return out
}

// Close stops every renewal goroutine and waits for them. It is NOT part of
// the SessionPresence interface — MemorySessionPresence has nothing to shut
// down — so callers hold the concrete type to call it. cmd/pad/cmd_server.go
// does exactly that during shutdown.
//
// Note what it deliberately does NOT do: it does not delete this instance's
// entries. A shutdown that raced a reconnect elsewhere would then delete a
// session that had already re-registered. Letting the TTL clear them is
// slower and correct.
func (p *RedisSessionPresence) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	for key, cancel := range p.renewals {
		delete(p.renewals, key)
		cancel()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func renewalKey(userID, sessionID string) string { return userID + "|" + sessionID }

func toAny(ss []string) []interface{} {
	out := make([]interface{}, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}
