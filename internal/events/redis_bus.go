package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/redisns"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// redisChannelSuffix is prepended to workspace IDs for Redis pub/sub
	// channels, under whatever namespace the installation configured —
	// "pad:events:" by default. See internal/redisns.
	redisChannelSuffix = "events:"

	// redisSeqSuffix names the global event sequence counter ("pad:event_seq"
	// by default). All instances share this counter so SSE event IDs are
	// globally ordered and Last-Event-ID is valid across any instance on
	// reconnect — which is also why RENAMING it (by introducing a namespace
	// on a running deployment) is a cutover rather than a config tweak:
	// the new counter starts from zero and connected clients' Last-Event-ID
	// values belong to the old space.
	redisSeqSuffix = "event_seq"

	// redisEpochSuffix identifies the CURRENT ID space ("pad:event_epoch" by
	// default), and exists because numeric detection alone cannot see a reset
	// that has already caught back up.
	//
	// A counter reset is detectable when an ID arrives at or below our
	// high-water mark. It is INVISIBLE when the new space has already climbed
	// past it: hold 100, lose the subscription, the counter resets and IDs
	// 1-101 are published, and the only one that reaches us is 101 — which
	// looks exactly like the contiguous successor of 100. The buffer then
	// mixes two ID spaces and a client resuming from OLD 100 is handed NEW 101,
	// having silently missed everything the old space had above 100.
	//
	// Same mechanism, same reasoning, and the same key shape as
	// internal/watchevents' watchevents_epoch. Deliberately a DISTINCT key
	// from that one for the same reason the counters are distinct: the two
	// buses carry independent Last-Event-ID spaces.
	redisEpochSuffix = "event_epoch"

	// redisDedupeSuffix namespaces the per-publish idempotency tokens.
	redisDedupeSuffix = "events:pub:"

	// redisDedupeTTLSeconds bounds how long a token is remembered. It only has
	// to cover a client-side retry burst — go-redis gives up after MaxRetries
	// with backoff measured in milliseconds — so a minute is generous, and the
	// keys are small and expire on their own.
	redisDedupeTTLSeconds = 60
)

// DEPLOYMENT SCOPING (BUG-2724). Every name above carries the installation's
// PAD_REDIS_NAMESPACE when one is set — see internal/redisns — and is
// byte-identical to the historical flat names when it is not. The rule, stated
// the same way in internal/watchevents and internal/server's presence registry
// because it belongs to all three at once: scoping comes from ONE shared
// config value built in cmd/pad/cmd_server.go, never from one package growing
// a prefix the others lack.
//
// STILL UNSCOPED, deliberately: Redis CLUSTER. No hash tags, and a non-cluster
// client. BUG-2736 adds a SECOND multi-key EVAL to the codebase — publishScript
// here spans four keys (sequence, channel, epoch, dedupe), matching the one in
// internal/watchevents — so a cluster port now has two call sites that would
// fail CROSSSLOT rather than one. Same deferral, one more site; BUG-2724 holds
// the reasoning for shipping tags only alongside a cluster client that can
// test them.

// publishScript assigns the ID and publishes in ONE atomic Redis call. It is
// PHASE 2 ONLY: an instance that has not been flipped still publishes through
// the two-call path in Publish, because the bare wire form carries the ID
// INSIDE the JSON and the JSON must therefore be marshalled after the ID is
// known. See config.EventsPublishEpoch for the rollout order.
//
// WHY ATOMIC. The two-call version does INCR and PUBLISH as separate
// round-trips, which lets two instances interleave — INCR 5, INCR 6, PUBLISH
// 6, PUBLISH 5 — so a receiving instance can append 6 before 5 and corrupt the
// ordering replayBuffer.since() assumes when it computes oldest and newest.
// That window is older than this fix and was already wrong; it becomes
// load-bearing here, because counter-backwards detection reads a descending ID
// as a RESET, and under the two-call version every interleave would look like
// one. Redis runs a script atomically on its single thread, so publish order
// equals ID order globally with no coordination on our side.
//
// THE EPOCH AND ID ARE PREPENDED as "<epoch>|<id>|<json>" rather than injected
// into the JSON. Two reasons, and the second is the one a future refactor to
// "cleaner JSON" would regress: string-editing JSON inside Lua is fragile, and
// an envelope object would be UNMARSHALLED SILENTLY by an older instance
// during a mixed roll — no matching keys, no error, a zero-valued Event
// delivered to that instance's clients. The prefix fails loudly instead. See
// decodePayload for the receiving side, which accepts both forms.
//
// The epoch is offered SET NX in steady state, so every publisher can propose
// a candidate and exactly the first one wins. The id == 1 branch is the
// deliberate exception and overwrites it — see that branch.
//
// A DEDUPE TOKEN, matching internal/watchevents' script. The mechanism it
// closes: go-redis retries a command whose reply was lost to a network error —
// a Redis failover being the obvious trigger — so the script can run, publish,
// and still return an error to its caller. Retried, it would publish the same
// event again under a second ID, and both copies look perfectly valid
// (ascending IDs, correct ordering), so nothing downstream can tell them
// apart. It is not merely a duplicate row: the web layout raises a toast for
// any externally-sourced item_created, so a duplicate is a duplicate toast
// plus a redundant fetch.
//
// SET NX on a caller-generated token turns the retry into a no-op: the retry
// carries the same KEYS[4], the SET fails, and the script returns 0 without
// publishing.
var publishScript = redis.NewScript(`
if redis.call('SET', KEYS[4], '1', 'NX', 'EX', ARGV[3]) == false then
  return 0
end
local id = redis.call('INCR', KEYS[1])
if id == 1 then
  -- The counter is starting from scratch: this installation's first publish
  -- ever, or the seq key was deleted or evicted under us. Both are a NEW id
  -- space, so the epoch is ROTATED rather than merely offered. Without this, a
  -- deleted seq key restarts the ids inside the SAME epoch and the epoch check
  -- reports nothing -- and the numeric check misses it too whenever a
  -- receiver's high-water mark is low enough that the restarted counter climbs
  -- past it before that receiver sees anything.
  redis.call('SET', KEYS[3], ARGV[2])
else
  -- Steady state: offer a candidate, and let the first publisher win.
  redis.call('SET', KEYS[3], ARGV[2], 'NX')
end
local epoch = redis.call('GET', KEYS[3])
redis.call('PUBLISH', KEYS[2], epoch .. '|' .. id .. '|' .. ARGV[1])
return id
`)

// RedisBus distributes events across multiple Pad instances via Redis pub/sub.
// Each instance subscribes to Redis channels for its locally-connected SSE clients,
// and publishes events to Redis so all instances see them.
type RedisBus struct {
	// observable carries the optional operational-event seam (BUG-2731).
	observable

	client *redis.Client

	// keys builds this installation's Redis names (BUG-2724). The zero
	// value is the historical un-namespaced keyspace, so a bus constructed
	// without one behaves exactly as it always did.
	keys redisns.Keys

	// mu guards subscriber membership, the per-workspace subscription
	// bookkeeping, AND the replay buffers TOGETHER.
	//
	// The buffers used to have their own RWMutex, and that separation is
	// what made BUG-2731's case 4 unfixable in isolation. stopRedisSubscription
	// does not JOIN the receive goroutine — sub.cancel() only signals it — so a
	// straggler already inside fanOutLocally could re-create a buffer we had
	// just dropped, with knownFrom set to its own ID, and the resulting
	// one-entry buffer would then vouch for coverage it never had. One lock
	// makes "is this workspace still being received?" and "append to its
	// buffer" a single atomic decision.
	//
	// Holding a write lock through the fan-out cannot STALL anyone, for the
	// reason internal/watchevents records for its own single-mutex design: the
	// sends are already non-blocking (select/default — a full channel is
	// dropped-and-logged, never awaited). It does serialize fan-outs, resumes
	// and map work against each other, which is a real cost — and is why the
	// subscriber map is indexed by workspace: the critical section is then
	// proportional to one workspace's subscribers rather than to all of them.
	mu sync.Mutex
	// subscribers is indexed BY WORKSPACE, not a flat set (codex round 6).
	// Fan-out runs under the same lock as everything else here, so scanning
	// every local subscriber to filter by workspace would make one hot
	// workspace's publish rate the serialization point for every other
	// workspace's fan-out AND for every resume. The index makes the critical
	// section O(subscribers in THIS workspace) with non-blocking sends.
	//
	// MemoryBus keeps its flat map deliberately: it has an RWMutex and a
	// separate replay lock, so its fan-out does not exclude anyone.
	subscribers map[string]map[chan Event]*subscriber
	// workspaceOf resolves a channel back to its workspace on Unsubscribe,
	// which is the one operation that has only the channel to go on.
	workspaceOf map[chan Event]string

	// Track which workspace channels we're subscribed to in Redis,
	// so we subscribe/unsubscribe as local SSE clients come and go.
	wsCounts map[string]int       // workspace → local subscriber count
	wsSubs   map[string]*redisSub // workspace → active Redis subscription

	// subGen numbers subscriptions so a message can be matched to the one it
	// arrived on. See fanOut for the race it closes.
	subGen int64

	// Per-workspace replay buffers for Last-Event-ID support.
	// Populated from events received via Redis pub/sub. Guarded by mu.
	replayBuffers map[string]*replayBuffer
	replaySize    int

	// publishEpoch selects the wire form this instance EMITS: the phase-2
	// "<epoch>|<id>|<json>" prefix when true, the historical bare JSON body
	// when false. Receiving accepts both regardless — see decodePayload and
	// config.EventsPublishEpoch for why emission is the half that is gated.
	publishEpoch bool

	// epoch is the ID space this instance has adopted, learned from arriving
	// messages rather than read at startup.
	//
	// AN OPAQUE TOKEN, NOT A NUMERIC BASE, and the asymmetry with MemoryBus is
	// deliberate rather than drift — see the `base` field on MemoryBus for the
	// full reasoning. In one sentence: this counter is SHARED across
	// processes, so no instance can compute an identity the others would
	// agree with, and the identity has to travel with the message instead. The
	// cost of that choice is that a cursor still carries no space of its own,
	// so an old and a new ID of the same value remain indistinguishable to a
	// resume; BUG-2736's trail names the numeric base that would close it and
	// why it is a follow-on unit. Empty until a prefixed message
	// arrives, which on a phase-1 deployment is never. Guarded by mu.
	//
	// LEARNED, NOT FETCHED, deliberately: reading the key at construction
	// would make an instance believe it belongs to a space whose events it has
	// not received, which is precisely the claim BUG-2731 spent a unit
	// removing. The epoch matters only in relation to buffered events, so it
	// arrives with them.
	epoch string

	// hadReset and discardedHighWater record that this bus has thrown a
	// sequence away, and how high the discarded buffers had climbed. Every
	// buffer built afterwards refuses cursors at or below that mark — see
	// newBuffer and dropAllBuffers, where the two reset reasons set them
	// differently on purpose. Guarded by mu.
	hadReset           bool
	discardedHighWater int64

	ctx    context.Context
	cancel context.CancelFunc
}

// redisSub tracks an active Redis subscription for a workspace.
type redisSub struct {
	pubsub *redis.PubSub
	cancel context.CancelFunc
	// gen identifies this subscription among all subscriptions this bus has
	// opened for the workspace, so a message carrying a stale gen can be
	// recognised as belonging to a subscription that has already ended.
	gen int64
}

// NewRedisBus creates a new Redis-backed EventBus.
// The provided redis.Client should already be configured and connected.
func NewRedisBus(client *redis.Client) *RedisBus {
	return NewRedisBusWithKeys(client, redisns.Default, false)
}

// NewRedisBusWithKeys is NewRedisBus with an explicit key namespace
// (BUG-2724). cmd/pad/cmd_server.go uses this one, passing the value
// shared with the watch bus and the presence registry so all three
// keyspaces carry the same namespace or none.
//
// publishEpoch selects the wire form this instance EMITS (BUG-2736). It is a
// constructor parameter with no default rather than a setter, so every call
// site states which phase of the rollout it is in and none can flip a bus that
// is already publishing. See config.EventsPublishEpoch for the order the two
// phases must be rolled in.
func NewRedisBusWithKeys(client *redis.Client, keys redisns.Keys, publishEpoch bool) *RedisBus {
	ctx, cancel := context.WithCancel(context.Background())
	return &RedisBus{
		client:        client,
		keys:          keys,
		publishEpoch:  publishEpoch,
		subscribers:   make(map[string]map[chan Event]*subscriber),
		workspaceOf:   make(map[chan Event]string),
		wsCounts:      make(map[string]int),
		wsSubs:        make(map[string]*redisSub),
		replayBuffers: make(map[string]*replayBuffer),
		replaySize:    DefaultReplayBufferSize,
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Subscribe registers a local subscriber for the given workspace.
// Starts a Redis subscription for the workspace if this is the first local subscriber.
func (b *RedisBus) Subscribe(workspaceID string) chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := b.addSubscriberLocked(workspaceID)
	if b.wsCounts[workspaceID] == 1 {
		// First local subscriber for this workspace — subscribe to Redis channel
		b.startRedisSubscription(workspaceID)
	}

	return ch
}

// addSubscriberLocked registers a channel for a workspace and bumps its local
// count. Callers must hold mu.
func (b *RedisBus) addSubscriberLocked(workspaceID string) chan Event {
	ch := make(chan Event, 64)
	byWorkspace, ok := b.subscribers[workspaceID]
	if !ok {
		byWorkspace = make(map[chan Event]*subscriber)
		b.subscribers[workspaceID] = byWorkspace
	}
	byWorkspace[ch] = &subscriber{ch: ch, workspaceID: workspaceID}
	b.workspaceOf[ch] = workspaceID
	b.wsCounts[workspaceID]++
	return ch
}

// SubscribeIfAllowed atomically checks the per-workspace limit and
// subscribes. See the EventBus interface for why there is no global limit
// here (BUG-2726).
//
// NOTE: the limit is enforced against local (per-pod) subscriber counts
// only. In multi-replica deployments the effective cap is multiplied by
// the number of replicas — as is every other streaming limit Pad has.
func (b *RedisBus) SubscribeIfAllowed(workspaceID string, maxPerWorkspace int) (chan Event, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if maxPerWorkspace > 0 && b.wsCounts[workspaceID] >= maxPerWorkspace {
		return nil, false
	}

	ch := b.addSubscriberLocked(workspaceID)
	if b.wsCounts[workspaceID] == 1 {
		b.startRedisSubscription(workspaceID)
	}

	return ch, true
}

// Unsubscribe removes a local subscriber and closes its channel.
// Cancels the Redis subscription if this was the last local subscriber for the workspace.
func (b *RedisBus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	wsID, ok := b.workspaceOf[ch]
	if !ok {
		return
	}
	delete(b.workspaceOf, ch)
	if byWorkspace, ok := b.subscribers[wsID]; ok {
		delete(byWorkspace, ch)
		if len(byWorkspace) == 0 {
			delete(b.subscribers, wsID)
		}
	}
	close(ch)

	b.wsCounts[wsID]--
	if b.wsCounts[wsID] <= 0 {
		delete(b.wsCounts, wsID)
		b.stopRedisSubscription(wsID)
	}
}

// Publish sends an event to Redis, which distributes it to all instances.
// Events are assigned a globally unique sequence ID via Redis INCR so that
// Last-Event-ID is valid across any instance on reconnect.
func (b *RedisBus) Publish(event Event) {
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().UnixMilli()
	}

	channel := b.keys.Name(redisChannelSuffix) + event.WorkspaceID
	if b.publishEpoch {
		b.publishWithEpoch(channel, event)
		return
	}

	// PHASE 1: the historical two-call path, unchanged. Assign a globally
	// ordered sequence ID via Redis atomic counter, so all instances share one
	// ID space and Last-Event-ID from one instance is meaningful on any other.
	//
	// It keeps the pre-existing interleave window (INCR and PUBLISH are two
	// round-trips, so two instances can publish out of ID order) and the
	// pre-existing retry-duplication window. Both are closed by phase 2 rather
	// than here, because the bare wire form carries the ID INSIDE the JSON and
	// the JSON therefore cannot be marshalled until the ID is known — which is
	// the whole reason the atomic script and the prefix arrive together.
	id, err := b.client.Incr(b.ctx, b.keys.Name(redisSeqSuffix)).Result()
	if err == nil && id == 1 {
		// THE COUNTER RESTARTED WHILE WE WERE PUBLISHING THE BARE FORM, so
		// any epoch left over from a previous phase-2 period now names a
		// space that no longer exists — and phase 2's rotation cannot fix it,
		// because that rotation is keyed on the script's own INCR returning 1
		// and by then the counter has already climbed past 1 under this path.
		//
		// The shape it closes (codex round 2): phase 2 mints epoch E and the
		// sequence reaches 500; the deployment rolls back to phase 1; the seq
		// key is then evicted or deleted; phase-1 publishers climb from 1
		// again; phase 2 is re-enabled and its SET NX finds E still there. A
		// receiver that had adopted E sees no change, and if its high-water
		// mark is below the new sequence — a replica that just started, or one
		// whose buffers were empty — the numeric check does not see the reset
		// either. Two ID spaces merge in one buffer silently, which is the one
		// outcome this whole unit exists to prevent.
		//
		// Deleting rather than rotating: this path has no epoch to propose
		// (it publishes no epoch at all), and an absent key is exactly what
		// phase 2's SET NX expects to find when it mints a new one.
		//
		// The cost, stated so it is not rediscovered as a bug: during the
		// phase-2 roll a phase-1 publisher could delete an epoch a flipped
		// publisher had just minted, costing ONE extra buffer drop when the
		// next epoch is minted. That is a loud, bounded resync, not a silent
		// merge — the direction this family always chooses.
		if delErr := b.client.Del(b.ctx, b.keys.Name(redisEpochSuffix)).Err(); delErr != nil {
			slog.Warn("events: the sequence restarted but the stale ID-space epoch could not be cleared; a later phase-2 publish may reuse it",
				"error", delErr)
		}
	}
	if err != nil {
		// NO LOCAL-COUNTER FALLBACK, and its removal is a fix rather than a
		// regression (BUG-2731). The previous version answered a failed INCR
		// by minting an id from a process-local counter starting at zero and
		// publishing it anyway — an id from a different space, which every
		// receiving instance reads as the counter having been reset. It also
		// bought nothing: this bus has no local fan-out path, so an event
		// that does not reach Redis reaches no subscriber on this instance
		// either, fallback id or not. Failing loudly is the honest outcome.
		slog.Error("failed to assign an event ID from Redis; dropping the publish", "error", err)
		return
	}
	event.ID = id

	data, err := json.Marshal(event)
	if err != nil {
		slog.Error("failed to marshal event for Redis", "error", err)
		return
	}

	if err := b.client.Publish(b.ctx, channel, data).Err(); err != nil {
		slog.Error("failed to publish event to Redis", "channel", channel, "error", err)
	}
}

// publishWithEpoch is the PHASE 2 path: one atomic script assigns the ID,
// maintains the epoch, and publishes "<epoch>|<id>|<json>".
//
// The event is marshalled with ID still zero, because the ID travels in the
// prefix and decodePayload writes it back onto the decoded Event. A receiver
// running phase 1 or phase 2 reads the same value either way; a receiver
// running a PRE-phase-1 binary cannot parse this at all, which is the reason
// the flip is a second roll rather than a config change (see
// config.EventsPublishEpoch).
func (b *RedisBus) publishWithEpoch(channel string, event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		slog.Error("failed to marshal event for Redis", "error", err)
		return
	}

	// A fresh token per logical publish — NOT per attempt, which is the point:
	// go-redis reuses the same arguments on its own retries, so the second run
	// of the script sees the same token and declines.
	dedupeKey := b.keys.Name(redisDedupeSuffix) + uuid.NewString()
	if err := publishScript.Run(b.ctx, b.client,
		[]string{b.keys.Name(redisSeqSuffix), channel, b.keys.Name(redisEpochSuffix), dedupeKey},
		string(data), uuid.NewString(), redisDedupeTTLSeconds).Err(); err != nil {
		// NO LOCAL-COUNTER FALLBACK, for the same reason the phase-1 path has
		// none (BUG-2731): an ID minted locally belongs to a different space,
		// which every receiving instance reads as a counter reset, and this
		// bus has no local fan-out path so the event reaches nobody here
		// either way.
		//
		// Note what this error does and does not mean: the script is atomic,
		// so it never half-executes — but go-redis retries a command whose
		// REPLY was lost, so an error here can accompany a publish that
		// actually happened. That is what the dedupe token is for, and why
		// this logs rather than re-publishing.
		slog.Error("failed to publish event to Redis", "channel", channel, "error", err)
	}
}

// EventsSince returns buffered events for a workspace with IDs greater than
// sinceID. Returns nil when this instance cannot vouch for the requested span
// — see the EventBus interface and replayBuffer.since.
func (b *RedisBus) EventsSince(workspaceID string, sinceID int64) []Event {
	// Reported once, on the way out, and with the lock released — see the
	// MemoryBus twin for why both no-buffer and refused-span must reach the
	// same counter.
	var missed []Event
	defer func() {
		if missed == nil {
			b.reportResumeGap(workspaceID)
		}
	}()

	b.mu.Lock()
	defer b.mu.Unlock()

	rb, ok := b.replayBuffers[workspaceID]
	if !ok {
		// No buffer means this instance is not currently covering the
		// workspace, or has only just started to. For a fresh client
		// (sinceID == 0) that is honestly "nothing to replay"; for a
		// resuming one it is the strongest form of "cannot vouch" there
		// is, and answering []Event{} told it that it was caught up
		// (BUG-2731).
		//
		// This is the NORMAL state for the first connection to a given
		// workspace on a given replica, because buffers are built lazily
		// in fanOutLocally — so a restart, a scale-up, or a namespace
		// cutover all land here, not just an exotic failure.
		if sinceID > 0 {
			return nil
		}
		missed = []Event{}
		return missed
	}
	missed = rb.since(sinceID)
	return missed
}

// Close shuts down all Redis subscriptions and closes local subscriber channels.
func (b *RedisBus) Close() {
	b.cancel() // signal all subscription goroutines to stop

	b.mu.Lock()
	defer b.mu.Unlock()

	for wsID, sub := range b.wsSubs {
		sub.cancel()
		sub.pubsub.Close()
		delete(b.wsSubs, wsID)
	}

	for wsID, byWorkspace := range b.subscribers {
		for ch := range byWorkspace {
			delete(b.workspaceOf, ch)
			close(ch)
		}
		delete(b.subscribers, wsID)
	}
}

// SubscriberCount returns the number of active local subscribers.
func (b *RedisBus) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.workspaceOf)
}

// WorkspaceSubscriberCount returns the number of active local subscribers for a workspace.
func (b *RedisBus) WorkspaceSubscriberCount(workspaceID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wsCounts[workspaceID]
}

// startRedisSubscription begins listening on a Redis channel for a workspace.
// Must be called with b.mu held.
func (b *RedisBus) startRedisSubscription(workspaceID string) {
	channel := b.keys.Name(redisChannelSuffix) + workspaceID
	pubsub := b.client.Subscribe(b.ctx, channel)

	subCtx, subCancel := context.WithCancel(b.ctx)
	b.subGen++
	gen := b.subGen
	b.wsSubs[workspaceID] = &redisSub{
		pubsub: pubsub,
		cancel: subCancel,
		gen:    gen,
	}

	go b.receiveMessages(subCtx, pubsub, workspaceID, gen)
}

// stopRedisSubscription cancels and cleans up the Redis subscription for a workspace.
// Must be called with b.mu held.
func (b *RedisBus) stopRedisSubscription(workspaceID string) {
	sub, ok := b.wsSubs[workspaceID]
	if !ok {
		return
	}
	sub.cancel()
	sub.pubsub.Close()
	delete(b.wsSubs, workspaceID)

	// WHEN WE STOP RECEIVING, THE HONEST STATE IS NO BUFFER, NOT A STALE
	// CONTIGUOUS ONE (BUG-2731). This is the invariant a future optimization
	// will be tempted to violate — keeping the buffer "in case they come
	// back" looks like a free win and is the bug.
	//
	// From here until the workspace is subscribed again, events published on
	// other instances never enter this buffer, while the buffer itself goes
	// on LOOKING complete: same IDs, no eviction, nothing to distinguish it
	// from a live one. A later client resuming with a cursor at or below the
	// stale newest ID would be replayed the stale tail and told nothing about
	// the hole. Dropping it makes the next resume answer nil, which is true.
	delete(b.replayBuffers, workspaceID)
}

// receiveMessages reads from a Redis pub/sub channel and fans out to local subscribers.
// receiveMessages consumes one workspace's Redis channel until the context is
// cancelled.
//
// IT USES ChannelWithSubscriptions, AND BOTH HALVES OF THAT CHOICE MATTER.
//
// Against Channel: a plain message channel hides RESUBSCRIPTIONS. go-redis
// reconnects transparently on a dropped connection — a Redis failover, a
// network blip, a server restart — and hands back the messages that arrive
// AFTER it recovers, saying nothing about the ones published while it was
// down. The replay buffer then carries a hole it has no idea about, and a
// resume across it is answered "caught up". A resubscription is the one form
// of mid-stream loss this process can actually OBSERVE (contrast BUG-2735,
// which stays open precisely because a message lost in transit leaves no local
// trace on a bus whose per-workspace IDs are non-consecutive by construction),
// so every subscription confirmation after the first ends that workspace's
// coverage, exactly like a stopped subscription.
//
// Against a bare Receive loop, which is what this was first written as: only
// the Channel* constructors start go-redis's health check (newChannel →
// initHealthCheck), and go-redis owns the redial and backoff, so a hand-rolled
// retry loop here is machinery this package does not need to maintain.
//
// WHAT NEITHER FORM DETECTS: a HALF-OPEN connection — no FIN, no RST, just a
// route that stopped working. Check it in the library rather than believing
// this comment: PubSub.Ping calls writeCmd and returns, never reading a reply
// (go-redis v9.22.0, pubsub.go), so the health check's error is nil for as
// long as the socket accepts writes — which a half-open socket does until its
// send buffer fills. The channel path sets no read deadline either
// (Receive → ReceiveTimeout(ctx, 0)).
//
// So an instance behind a wedged route sits there receiving nothing while its
// buffer keeps looking valid. Detecting that needs application-level idle
// tracking, which is BUG-2730's family and its own decision, because it needs
// a threshold. Do not assume the health check covers it.
func (b *RedisBus) receiveMessages(ctx context.Context, pubsub *redis.PubSub, workspaceID string, gen int64) {
	defer b.reportReceiveLoopExited()

	ch := pubsub.ChannelWithSubscriptions()
	var subscribed bool
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-ch:
			if !ok {
				return
			}
			switch msg := raw.(type) {
			case *redis.Subscription:
				if msg.Kind != "subscribe" && msg.Kind != "psubscribe" {
					continue
				}
				if !subscribed {
					subscribed = true
					continue
				}
				// A RESUBSCRIPTION: the connection dropped and came back, and
				// whatever was published in between never reached us.
				slog.Warn("events: pub/sub resubscribed; dropping this workspace's replay buffer, resumes across the gap will report sync_required",
					"workspace", workspaceID, "channel", msg.Channel)
				b.dropWorkspaceCoverage(workspaceID, ResetReasonSubscriptionResumed, gen)

			case *redis.Message:
				epoch, event, err := decodePayload(msg.Payload)
				if err != nil {
					slog.Error("failed to decode Redis event", "channel", msg.Channel, "error", err)
					continue
				}
				b.fanOutFromRedis(gen, epoch, event)
			}
		}
	}
}

// dropWorkspaceCoverage ends this instance's coverage of one workspace,
// because something happened that its buffer cannot account for. The next
// resume for that workspace answers nil until coverage is re-established.
//
// Scoped to the ONE workspace rather than all of them, unlike an ID-space
// reset: a dropped subscription says nothing about the shared counter or about
// any other workspace's channel, and dropping the rest would be a resync
// charged to clients whose stream never broke.
func (b *RedisBus) dropWorkspaceCoverage(workspaceID, reason string, gen int64) {
	var report string
	defer func() {
		if report != "" {
			b.reportReset(report)
		}
	}()

	b.mu.Lock()
	defer b.mu.Unlock()

	// THE GENERATION CHECK BELONGS HERE TOO, not only in fan-out (codex round
	// 7). A receive loop can notice its connection died LONG after the
	// workspace was unsubscribed and resubscribed under it: last viewer
	// leaves, the buffer is dropped and the subscription torn down, a viewer
	// returns and a new generation starts buffering — and only then does the
	// old goroutine reach this line and delete the REPLACEMENT buffer. The
	// returning client is then told sync_required for an outage that ended
	// before its subscription began, and the reset counter names an incident
	// that did not happen to it.
	if sub, ok := b.wsSubs[workspaceID]; !ok || sub.gen != gen {
		return
	}

	if _, ok := b.replayBuffers[workspaceID]; !ok {
		// Nothing buffered: there is no coverage to end and no client that
		// could have been told it was current. Reporting here would give the
		// counter a baseline on every reconnect of an idle workspace.
		return
	}
	delete(b.replayBuffers, workspaceID)
	report = reason
}

// currentSubGen reports the generation of the workspace's live subscription,
// or 0 if it has none. Test seam: a real message carries its generation down
// from receiveMessages, and a test driving the fan-out directly needs a way to
// name the same value rather than guessing it.
func (b *RedisBus) currentSubGen(workspaceID string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sub, ok := b.wsSubs[workspaceID]; ok {
		return sub.gen
	}
	return 0
}

// buffersHoldEvents reports whether any replay buffer has been written to.
// Callers must hold mu.
func (b *RedisBus) buffersHoldEvents() bool {
	for _, rb := range b.replayBuffers {
		if rb.count > 0 {
			return true
		}
	}
	return false
}

// newBuffer builds a replay buffer of the right flavour for this bus's
// history: once a sequence has been discarded under us, every buffer we build
// afterwards must refuse the cursor immediately below its first event, because
// that cursor may belong to the sequence we threw away. Callers must hold mu.
func (b *RedisBus) newBuffer() *replayBuffer {
	if b.hadReset {
		return newReplayBufferAfterReset(b.replaySize, b.discardedHighWater)
	}
	return newReplayBuffer(b.replaySize)
}

// dropAllBuffers throws away every replay buffer because this instance can no
// longer vouch for the sequence they describe — either the ID space itself
// changed, or an ID arrived at or below the high-water mark. Callers must hold
// mu.
//
// raiseFloor says whether the replacements should additionally refuse every
// cursor at or below what the discarded buffers held, and the two reset
// reasons answer it differently ON PURPOSE. Getting this backwards produces a
// RESYNC LOOP, which is worse than the bug the floor exists to fix.
//
//   - COUNTER BACKWARDS, no epoch change: the arriving ID is in the SAME
//     numeric space we were already tracking (whoever published it INCRed the
//     same key). We just discarded events a cursor can legitimately ask about,
//     and the successor of such a cursor is exactly what we threw away — so
//     the floor is raised. The case this really covers is a mixed-version or
//     mixed-FORMAT roll, where a phase-1 publisher's non-atomic INCR/PUBLISH
//     delivers a LOWER ID after a newer one.
//
//   - EPOCH CHANGE: a genuinely NEW space, typically restarting from 1 while
//     the dead space had climbed high. Raising the floor there would refuse
//     every cursor until the new counter passed the old high-water mark — and
//     since each refusal hands the client a FRESH low cursor that is refused
//     again, that is a loop, not one resync. The ambiguity between an old and
//     a new ID of the same value is accepted instead, exactly as
//     internal/watchevents accepts it; see BUG-2736's trail for the numeric
//     base that would close it and why it is not this unit.
//
// An epoch change also CLEARS any standing floor, which is not the same as
// declining to raise one. The floor is a same-space device: it names IDs whose
// successors we discarded FROM THE SPACE WE WERE TRACKING. Once the space
// itself is gone those numbers mean nothing, and leaving the floor standing
// produces the very loop the paragraph above rules out — just reached from a
// bus that took a counter-backwards reset earlier in its life.
func (b *RedisBus) dropAllBuffers(raiseFloor bool) {
	if raiseFloor {
		for _, rb := range b.replayBuffers {
			if rb.lastAppendedID > b.discardedHighWater {
				b.discardedHighWater = rb.lastAppendedID
			}
		}
	} else {
		b.discardedHighWater = 0
	}
	b.hadReset = true
	b.replayBuffers = make(map[string]*replayBuffer)
}

// decodePayload parses the "<epoch>|<id>|<json>" wire form publishScript emits,
// and ALSO accepts a bare JSON body with no prefix.
//
// THE BARE FORM IS NOT LEGACY-ONLY. It is what every phase-1 instance
// publishes — which, until an operator flips config.EventsPublishEpoch, is
// every instance — as well as what a pre-BUG-2736 binary publishes. Accepting
// it is what makes both rolls zero-loss in the new-receiving-old direction. It
// returns an empty epoch, which the receive path reads as "no ID-space
// information" and leaves the epoch bookkeeping untouched rather than treating
// it as a change.
//
// The reverse direction is not recoverable from this side: an instance running
// a PRE-phase-1 binary fails to unmarshal a prefixed payload and drops the
// event for its own clients, loudly. That asymmetry is the entire reason the
// flip is a second roll. See docs/deployment.md.
//
// Splitting on the FIRST two separators keeps a '|' inside the JSON body
// harmless: the epoch is a uuid and the ID is digits, so neither can contain
// one. The leading '{' check is what stops a JSON body that happens to contain
// two '|' characters from being mistaken for a prefixed payload — an epoch is
// never a JSON object.
func decodePayload(payload string) (string, Event, error) {
	if parts := strings.SplitN(payload, "|", 3); len(parts) == 3 && !strings.HasPrefix(parts[0], "{") {
		epoch, idPart, body := parts[0], parts[1], parts[2]
		if epoch == "" {
			return "", Event{}, fmt.Errorf("payload has an empty epoch prefix")
		}
		id, err := strconv.ParseInt(idPart, 10, 64)
		if err != nil {
			return "", Event{}, fmt.Errorf("payload id prefix %q is not an integer: %w", idPart, err)
		}
		var event Event
		if err := json.Unmarshal([]byte(body), &event); err != nil {
			return "", Event{}, fmt.Errorf("payload body is not an Event: %w", err)
		}
		event.ID = id
		return epoch, event, nil
	}

	var event Event
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return "", Event{}, fmt.Errorf("payload is neither <epoch>|<id>|<json> nor a bare Event: %w", err)
	}
	return "", event, nil
}

// fanOutFromRedis is the receive path: a message that arrived on the
// subscription identified by gen, carrying the ID space it belongs to.
//
// An empty epoch means the payload carried no ID-space information (a phase-1
// or pre-BUG-2736 publisher, or a direct test call) and the bookkeeping is left
// alone — silence is not evidence of a change.
func (b *RedisBus) fanOutFromRedis(gen int64, epoch string, event Event) {
	b.fanOut(gen, epoch, event)
}

// fanOutLocally distributes an event to all local subscribers for the event's
// workspace and stores it in the replay buffer, with no id-space information.
func (b *RedisBus) fanOutLocally(event Event) {
	b.fanOut(anySubscription, "", event)
}

// anySubscription opts out of the generation check for callers that are not a
// receive loop — tests driving the fan-out directly. A real message always
// carries the generation of the subscription it arrived on.
const anySubscription int64 = 0

func (b *RedisBus) fanOut(gen int64, epoch string, event Event) {
	// Registered FIRST so it runs LAST — after the Unlock below, so an
	// observer may call back into the bus without deadlocking the receive
	// loop.
	var reset string
	defer func() {
		if reset != "" {
			b.reportReset(reset)
		}
	}()

	// Events received via Redis pub/sub already carry a global ID assigned by
	// the publishing instance via Redis INCR. We use that ID directly so all
	// instances share the same ID space for Last-Event-ID replay.
	b.mu.Lock()
	defer b.mu.Unlock()

	// FAN-OUT REFUSES TO APPEND FOR A WORKSPACE WITH NO LIVE SUBSCRIPTION
	// (BUG-2731). sub.cancel() in stopRedisSubscription signals the receive
	// goroutine; it does not join it, so a message already in flight can
	// arrive here after the workspace was dropped. Appending it would
	// re-create the buffer with knownFrom set to that straggler's ID — a
	// one-entry buffer vouching for coverage that ended when the
	// subscription did, which is precisely the state the drop exists to
	// prevent.
	//
	// Checked under the same lock that guards wsSubs, so the answer cannot
	// go stale between the check and the append.
	sub, receiving := b.wsSubs[event.WorkspaceID]
	if !receiving {
		return
	}
	// ...AND THAT THIS MESSAGE BELONGS TO THE SUBSCRIPTION THAT IS LIVE NOW
	// (codex round 1 F2). Checking only that SOME subscription exists leaves
	// the same hole one step further along: last subscriber leaves, buffer
	// dropped, a NEW subscriber arrives and registers a new subscription, and
	// only THEN does the old receive goroutine's in-flight message land. It
	// would be appended, setting knownFrom to a pre-outage ID, so a resume
	// from that ID is answered "caught up" while the outage's events are gone.
	if gen != anySubscription && sub.gen != gen {
		return
	}

	// ID-SPACE RECONCILIATION (BUG-2736). Both checks answer the same question
	// — do the events already buffered belong to the same sequence as this
	// one? — and both are needed, because neither sees the other's case. The
	// epoch catches a reset that has already climbed past our high-water mark,
	// which is numerically invisible; the high-water check catches a reset on
	// an instance that never learned the previous epoch, including one
	// publishing from a phase-1 or pre-BUG-2736 binary.
	//
	// Every buffer is dropped, not just this workspace's: the counter is
	// global, so a reset invalidates all of them at once.
	if epoch != "" && b.epoch != epoch {
		// ADOPTING AN EPOCH ONTO A NON-EMPTY BUFFER IS ALSO A RESET. Learning
		// an epoch for the first time normally means the first message of this
		// bus's life, and dropping empty buffers would be pointless. But
		// during the phase-2 roll the buffers can already hold events from a
		// phase-1 publisher, whose payloads carry no epoch at all — and those
		// events' ID space is exactly what we have no way to compare against
		// the one we are now being told about.
		//
		// The dangerous shape: bare events up to 5, the counter is then
		// deleted, a flipped publisher rotates the epoch and climbs to 6
		// before this instance receives anything. 6 exceeds our high-water
		// mark, so the numeric check sees an ordinary successor, and without
		// this branch the two spaces merge in one buffer.
		//
		// Costs at most ONE drop per instance per roll: once adopted, later
		// bare messages leave the epoch alone and later prefixed ones match.
		//
		// WHAT THIS DELIBERATELY DOES NOT COVER: a bus whose buffers are
		// EMPTY adopts without dropping, so its first buffer starts at exactly
		// the first ID it sees. A client holding the ID one below that — from
		// a space this process never saw, because it started through a
		// cutover — is then served. Closing it locally means every bus
		// refusing the adjacent cursor forever, which trades a RARE silent
		// skip for a COMMON extra resync: on a multi-instance deployment a
		// client legitimately holds ID 149 from replica A and reconnects to
		// replica B whose first ID for that workspace is 150, and consecutive
		// global IDs land in the same busy workspace routinely. BUG-2736's
		// trail names the numeric-base design that closes it with neither
		// cost, and why it is a follow-on rather than this unit.
		if b.epoch != "" || b.buffersHoldEvents() {
			slog.Warn("event ID space changed; dropping replay buffers, resumes spanning the change will report sync_required",
				"previous_epoch", b.epoch, "new_epoch", epoch, "id", event.ID)
			b.dropAllBuffers(false)
			reset = ResetReasonEpochChange
		}
		b.epoch = epoch
	}

	// Store in replay buffer for reconnect replay.
	rb, ok := b.replayBuffers[event.WorkspaceID]
	if !ok {
		rb = b.newBuffer()
		b.replayBuffers[event.WorkspaceID] = rb
	}
	if rb.lastAppendedID != 0 && event.ID <= rb.lastAppendedID {
		// THIS WHOLE MECHANISM IS TRANSITIONAL. READ THIS BEFORE TUNING IT.
		//
		// Once every publisher is flipped to phase 2, publish order equals ID
		// order globally, and a genuine counter restart rotates the epoch (see
		// publishScript's id == 1 branch), which is a different code path
		// entirely. So a backwards ID in steady state is an ANOMALY, not a
		// case needing clever classification. Its real job is the ROLL WINDOW,
		// where a phase-1 publisher's non-atomic INCR/PUBLISH can deliver a
		// lower ID after a higher one.
		//
		// IT DOES NOT GO AWAY WHEN THE FORMAT FLIP COMPLETES, which the
		// original scoping hoped it would. The trigger is mixed-VERSION
		// ORDERING — older binaries assign and publish in two calls — not
		// mixed-format payloads, so publish-old-until-flip removes the
		// mixed-FORMAT window only. The floor lives for as long as any
		// deployment can run two publisher versions at once, which is every
		// rolling upgrade.
		//
		// It has already been tuned four times, each round finding a defect
		// inside the previous round's cleverness. So the floor is raised
		// UNCONDITIONALLY: every cursor at or below what the discarded buffers
		// held is refused. The alternative was a discriminator on "a restart
		// always begins at 1", which an out-of-order ID 1 during a roll
		// defeats — silently. Refusing too much is loud: it shows up in
		// pad_event_resume_gaps_total and in the warning below. Refusing too
		// little loses events with nothing to show for it.
		//
		// THE COST, stated so nobody rediscovers it as a bug: if a phase-1
		// publisher restarts the counter mid-roll, cursors are refused until
		// the sequence climbs past the dead high-water mark, so affected
		// clients resync repeatedly. Bounded by the roll — once every
		// publisher is flipped, restarts rotate the epoch, and an epoch change
		// CLEARS the floor (see dropAllBuffers).
		slog.Warn("event sequence went backwards; dropping replay buffers, resumes below the discarded high-water mark will report sync_required",
			"high_water_mark", rb.lastAppendedID, "id", event.ID, "workspace", event.WorkspaceID)
		b.dropAllBuffers(true)
		rb = b.newBuffer()
		b.replayBuffers[event.WorkspaceID] = rb
		reset = ResetReasonCounterBackward
	}
	rb.append(event)

	for _, sub := range b.subscribers[event.WorkspaceID] {
		select {
		case sub.ch <- event:
		default:
			// A DROP HERE IS SILENT TO THE SUBSCRIBER, and BUG-2731
			// deliberately did not change that — see BUG-2730, which owns it
			// for both buses. Everything BUG-2731 made honest is per-WORKSPACE
			// state evaluated when a client asks (cold start, stopped
			// subscription, reconnect, ID-space reset); this is per-SUBSCRIBER
			// state discovered mid-fan-out about a connection that is still
			// open, so telling it needs a channel from the bus to one live
			// consumer that neither bus has.
			//
			// The consequence, so nobody reads this as harmless: a later
			// delivered event advances that client's Last-Event-ID PAST the
			// dropped IDs, after which no replica will replay them, because
			// every replica agrees the cursor is current.
			slog.Warn("dropping event for slow subscriber", "type", event.Type, "workspace", event.WorkspaceID)
		}
	}
}
