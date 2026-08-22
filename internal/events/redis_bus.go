package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/redisns"
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
)

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
	return NewRedisBusWithKeys(client, redisns.Default)
}

// NewRedisBusWithKeys is NewRedisBus with an explicit key namespace
// (BUG-2724). cmd/pad/cmd_server.go uses this one, passing the value
// shared with the watch bus and the presence registry so all three
// keyspaces carry the same namespace or none.
func NewRedisBusWithKeys(client *redis.Client, keys redisns.Keys) *RedisBus {
	ctx, cancel := context.WithCancel(context.Background())
	return &RedisBus{
		client:        client,
		keys:          keys,
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
	// Assign a globally ordered sequence ID via Redis atomic counter, so all
	// instances share one ID space and Last-Event-ID from one instance is
	// meaningful on any other.
	id, err := b.client.Incr(b.ctx, b.keys.Name(redisSeqSuffix)).Result()
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
// WHAT NEITHER FORM DETECTS, measured rather than assumed: a HALF-OPEN
// connection — no FIN, no RST, just a route that stopped working. The health
// check's PubSub.Ping only WRITES the command and never reads a reply
// (go-redis v9.22.0, pubsub.go), so it succeeds for as long as the socket
// accepts writes, and the channel path sets no read deadline. A proxy that
// silently stops forwarding produced no reconnect in 24 seconds of probing.
// So an instance behind a wedged route still sits there receiving nothing
// while its buffer keeps looking valid; detecting that needs application-level
// idle tracking, which is BUG-2730's family and its own decision. Do not
// assume the health check covers it.
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
				var event Event
				if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
					slog.Error("failed to unmarshal Redis event", "channel", msg.Channel, "error", err)
					continue
				}
				b.fanOutFromRedis(gen, event)
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

// fanOutFromRedis is the receive path: a message that arrived on the
// subscription identified by gen.
func (b *RedisBus) fanOutFromRedis(gen int64, event Event) {
	b.fanOut(gen, event)
}

// fanOutLocally distributes an event to all local subscribers for the event's
// workspace and stores it in the replay buffer, with no id-space information.
func (b *RedisBus) fanOutLocally(event Event) {
	b.fanOut(anySubscription, event)
}

// anySubscription opts out of the generation check for callers that are not a
// receive loop — tests driving the fan-out directly. A real message always
// carries the generation of the subscription it arrived on.
const anySubscription int64 = 0

func (b *RedisBus) fanOut(gen int64, event Event) {
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

	// Store in replay buffer for reconnect replay.
	rb, ok := b.replayBuffers[event.WorkspaceID]
	if !ok {
		rb = newReplayBuffer(b.replaySize)
		b.replayBuffers[event.WorkspaceID] = rb
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
