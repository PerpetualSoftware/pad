// Package watchevents is the internal notification pipeline behind the
// padd watch / nudge feature (TASK-2533, per DOC-2479's event-model
// design). Producers (item update, item move, comment creation, ...)
// publish a Notification whenever something watch-worthy happens;
// GET /api/v1/events/stream is the single consumer, filtering the raw
// stream down to each caller's own watches plus the addressed-to-you
// predicate before anything reaches a client (DR-2: no firehose, no
// wildcard subscriptions).
//
// TWO IMPLEMENTATIONS, PICKED BY DEPLOYMENT SHAPE. MemoryBus is an
// in-process pub/sub, exactly like internal/events.MemoryBus, and it has
// the same blind spot: a Notification published on one process is never
// seen by a stream connection held open on a different process, and is
// silently dropped for that connection. RedisBus (BUG-2651, redis_bus.go)
// closes that by routing every notification through one shared Redis
// channel. cmd/pad/cmd_server.go picks between them on PAD_REDIS_URL,
// the same switch the event bus already uses — so a self-hosted
// single-process binary keeps MemoryBus and never grows a Redis
// dependency.
//
// Bus was defined as an interface from Phase 1 precisely so a second
// implementation could slot in without touching any producer or the stream
// handler, and that held for RedisBus: it changed neither.
//
// It did NOT hold for BUG-2699, and the distinction is worth keeping
// (codex round 6). Making Publish report acceptance changed the INTERFACE,
// not just the implementations, so every producer had to be revisited —
// not to change behaviour, but to rule on what each should do with an
// answer it had never been given. An interface absorbs a new
// implementation; it does not absorb a new question.
//
// PRESENCE IS NO LONGER THE ODD ONE OUT. internal/server's
// SessionPresence registry used to stay per-process after this bus went
// shared, and the combination was worse than either being consistent: a
// SESSION-TARGETED push is gated on that registry BEFORE publishing
// (handlers_push.go), so a push for a session held on another instance
// was skipped rather than carried, and GET /api/v1/sessions could not
// offer it as a target at all. BUG-2698 closed that with
// RedisSessionPresence, on the same PAD_REDIS_URL switch. All three —
// event bus, watch bus, presence registry — now cross instance
// boundaries together or none of them do.
//
// See internal/server/session_presence.go for that registry's own
// reaping story, which this package's Redis bus does not need: a
// notification is transient, while a presence entry outlives the process
// that wrote it and has to expire on its own.
package watchevents

import (
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrBusClosed is returned by Publish when the bus has already been shut
// down (BUG-2699). It is the ONE Publish failure that proves the
// notification was not published: Close has already emptied the
// subscriber set, so nothing could have received it and nothing can.
//
// It exists as a sentinel rather than an ordinary error because callers
// act on the distinction — see Bus.Publish's doc comment on why every
// other error means "unconfirmed" instead. Compare with errors.Is; both
// implementations wrap it with context.
var ErrBusClosed = errors.New("watchevents: bus is closed")

// Notification kinds, matching DOC-2479's event payload contract
// (kind ∈ {status-change, assignment, comment, ask}) exactly.
const (
	KindStatusChange = "status-change"
	KindAssignment   = "assignment"
	KindComment      = "comment"
	// KindAsk is contract-reserved: DOC-2479 specs it as part of the kind
	// enum, but no Phase 1 producer emits it. Grounding "human-gate-shaped
	// collection" mechanically requires either a collection `kind` field
	// or a user→active-role binding, and this codebase has neither today
	// (TASK-2533 plan review, confirmed with the dispatcher). Left in the
	// enum so the wire contract doesn't have to change when a producer
	// eventually exists.
	KindAsk = "ask"
	// KindPush is IDEA-2544 Phase 1's human→harness addressed-dispatch
	// event: POST .../items/{itemSlug}/push accepts one of these,
	// self-addressed (TargetUserID == the pushing user), any time a user
	// wants to put an item + instruction in front of their own harness
	// right now rather than waiting on assignment/watch semantics. NOT
	// "publishes exactly one" unconditionally as of PLAN-2558 S5
	// (TASK-2588): handlePushToItem decides whether to publish at all —
	// a session-targeted request whose id matches no live session in the
	// presence registry skips the publish entirely. That registry is
	// per-process with MemoryBus and SHARED with RedisBus (BUG-2698), so
	// the skip means "nothing is listening anywhere" in both shapes — an
	// earlier version of this sentence said LOCAL, which described the
	// window between BUG-2651 and BUG-2698 (codex round 24). See
	// handlers_push.go's own comment on that gate, plus TargetSessionID
	// and pushResponse.DeliveredSessions there.
	// KindAsk is push's reserved sibling in the other direction
	// (harness→human) — see TargetUserID's doc comment for the shared
	// envelope shape the two are meant to converge on.
	KindPush = "push"
)

// Notification is one watch-worthy fact: an item's status changed, it was
// (re)assigned, or a comment landed on it. Consumed only by the
// GET /api/v1/events/stream handler, which filters and reshapes it per
// connected caller before anything is serialized to an HTTP response —
// this type itself is never sent to a client as-is.
type Notification struct {
	// ID is a monotonic sequence number assigned by the Bus on Publish,
	// used for Last-Event-ID resume. Zero until Publish assigns it.
	ID          int64
	WorkspaceID string
	ItemID      string
	// CollectionID is the watched/assigned item's collection, needed so
	// the stream handler can apply the SAME current-access visibility
	// check to EVERY notification kind — watch-matched and
	// addressed-to-you alike (TASK-2533 codex round 2 finding 2: the
	// addressed-to-you branch used to skip this check entirely).
	CollectionID string
	// ItemRef is the human-facing issue ID ("TASK-214"), pre-resolved by
	// the producer so the stream handler and the CLI's --for-session
	// formatter never need a second lookup.
	ItemRef   string
	Kind      string
	Actor     string
	ActorName string
	Summary   string
	// AssignedUserID is populated on Kind == KindAssignment with the
	// item's NEW assignee (empty string = unassigned). It no longer gates
	// delivery: IDEA-2544 Phase 2 (TASK-2551) dropped assignment from the
	// addressed-to-you stream entirely — assignment is bookkeeping, push
	// is dispatch — so a KindAssignment notification now reaches a caller
	// only via an explicit watch on the item, exactly like every other
	// item-level fact (see server.watchNotificationVisible). Producers
	// keep populating it: it's the assignment's payload, it costs
	// nothing, and any future opt-in re-addressing (a per-watch flag, a
	// config key) needs it in place to be a consumer-side change only.
	AssignedUserID string
	// TargetUserID is IDEA-2544's generalized addressed-to field:
	// populated on Kind == KindPush with the user the push is addressed
	// to (Phase 1 always sets this to the pushing user's own ID —
	// self-addressed only, per Dave's product call that pushing into
	// someone else's session is a consent question, not a code
	// question). watchNotificationVisible compares this directly against
	// the connected caller's user ID, exactly like AssignedUserID's role
	// for KindAssignment. Reserved KindAsk is expected to share this same
	// field (harness→human addressed traffic) rather than growing its
	// own, once it has a producer. Deliberately NOT part of
	// watchEventPayload's wire shape (server.go) — it exists purely to
	// gate delivery server-side; a client never needs to know who else a
	// notification could have been addressed to, and echoing it back
	// would leak a user ID for no consumer that needs it.
	TargetUserID string
	// TargetSessionID narrows TargetUserID to one of that user's live
	// event-stream connections (PLAN-2558 S5, TASK-2588). Populated only
	// on Kind == KindPush, and only when the pusher named a specific
	// session id from the S1 presence registry (GET /api/v1/sessions);
	// empty means "every one of TargetUserID's sessions that the delivery
	// predicate admits" — armed, and able to see the item — rather than
	// every CONNECTED one; broadcast is targeted-with-an-empty-SESSION-
	// predicate, not a separate code path and not an unfiltered one. watchNotificationVisible checks this against the
	// SAME session id session_presence.go handed the connection at
	// Add() time, exactly parallel to how TargetUserID gates against
	// the connected caller's user id. An id that names no live session
	// (vanished, mistyped, or — deliberately indistinguishable from
	// either — belonging to a DIFFERENT user) simply matches nothing:
	// there is no separate "not found" signal here, by design, because
	// this field must never become an existence oracle across users.
	TargetSessionID string
	// StatusFieldKey / ToStatus are populated on Kind == KindStatusChange,
	// mirroring models.ItemMutationSignal, so the `--until field=value`
	// watch predicate can be evaluated against a Notification directly
	// without a second lookup back into the item.
	StatusFieldKey string
	ToStatus       string
	Timestamp      int64
}

// Bus is the pub/sub surface the watch pipeline's producers and the
// GET /api/v1/events/stream consumer share. See the package doc comment
// for which implementation serves which deployment shape.
type Bus interface {
	// Publish assigns the Notification a monotonic ID, stores it in the
	// replay buffer, and fans it out to every live Subscribe channel.
	// ID assignment and buffer insertion are atomic under one lock
	// (codex round 1 finding 4): two concurrent Publish calls that got
	// their IDs from separate critical sections could otherwise append
	// to the replay buffer out of ID order, corrupting since()'s
	// ordering assumptions.
	//
	// REPORTS ACCEPTANCE, NOT DELIVERY (BUG-2699). A nil error means the
	// bus took ownership of the notification; it says nothing about
	// whether any subscriber read it, and there is still no ack from the
	// receiving side. Most producers publish best-effort on top of a
	// durable store write and should discard the result deliberately —
	// internal/server's publishWatchNotification helper is where that
	// ruling lives. The push endpoint is the one caller that acts on it,
	// because a push has no persistence to recover from.
	//
	// A RETURNED ERROR IS TWO DIFFERENT OUTCOMES, and a caller that acts
	// on one must not collapse them:
	//
	//   - ErrBusClosed means nothing was published, provably. The bus was
	//     already shut down; no subscriber can ever have seen it. Safe to
	//     report as a clean refusal and safe for the caller to retry
	//     against a live bus.
	//   - Any OTHER error means UNCONFIRMED, not "did not happen".
	//     RedisBus's publish is a Lua script call, and go-redis retries a
	//     command whose reply was lost to a network error — which is why
	//     that script carries a dedupe token at all (see redis_bus.go's
	//     publishScript). The script may have run and published while the
	//     call still returns an error, so re-publishing risks a DUPLICATE
	//     dispatch rather than a repeat of nothing.
	Publish(n Notification) error
	// Subscribe returns a channel that receives every future
	// Notification, with NO replay. There is exactly one logical stream
	// (unlike internal/events.EventBus, which is workspace-scoped) —
	// per-caller filtering happens in the consumer, not the bus, per
	// DR-2. Use this for a fresh (non-resuming) connection; use
	// SubscribeAndReplaySince for a Last-Event-ID resume.
	Subscribe() chan Notification
	// SubscribeAndReplaySince atomically subscribes AND captures every
	// buffered notification with ID > sinceID, under the SAME lock
	// (codex round 1 finding 3). Subscribing and reading the replay
	// buffer as two separate calls leaves a window where a Notification
	// published in between lands in BOTH the replay set and the live
	// channel — this closes that window structurally rather than
	// requiring the consumer to dedupe by ID. Returns (ch, nil) if
	// sinceID has been evicted from the buffer (gap too large — caller
	// should treat this like the SSE handler's sync_required signal);
	// the subscription is still valid in that case, only the replay is
	// unavailable.
	SubscribeAndReplaySince(sinceID int64) (chan Notification, []Notification)
	// Unsubscribe removes a subscriber and closes its channel.
	Unsubscribe(ch chan Notification)
	// EventsSince returns buffered notifications with ID > sinceID, for
	// Last-Event-ID resume. Returns nil if sinceID has been evicted from
	// the buffer (gap too large — caller should treat this like the SSE
	// handler's sync_required signal). Exposed as a standalone primitive
	// for tests and any future caller that doesn't need the atomic
	// subscribe-and-replay guarantee; GET /api/v1/events/stream uses
	// SubscribeAndReplaySince instead, precisely to avoid that gap.
	EventsSince(sinceID int64) []Notification
	// Close shuts the bus down, closing every subscriber channel.
	Close()
}

// DefaultReplayBufferSize and DefaultReplayMaxAge mirror
// internal/events' defaults. The watch stream is lower-volume than the
// per-workspace activity firehose (only status/assignment/comment
// notifications that matter for *someone's* watch or addressed-to-you
// state), so the same sizing is a reasonable starting point without
// needing independent tuning.
const (
	DefaultReplayBufferSize = 1024
	DefaultReplayMaxAge     = 5 * time.Minute
)

// replayEntry pairs a Notification with the time it was published, so
// the ring buffer can also enforce DefaultReplayMaxAge (unused for now —
// eviction is purely capacity-driven, exactly like internal/events —
// but kept alongside the Notification for parity/future use).
type replayBuffer struct {
	items []Notification
	size  int
	head  int
	count int
}

func newReplayBuffer(size int) *replayBuffer {
	return &replayBuffer{items: make([]Notification, size), size: size}
}

func (rb *replayBuffer) append(n Notification) {
	rb.items[rb.head] = n
	rb.head = (rb.head + 1) % rb.size
	if rb.count < rb.size {
		rb.count++
	}
}

// since is this package's replay-window query, scoped to Notification.
//
// It NO LONGER MIRRORS internal/events.replayBuffer.since, and the difference
// is deliberate rather than drift: that one also applies a knownFrom coverage
// check inside `since`, because its buffers are per-workspace and fed by a
// global counter. Here the coverage question is answered one level up —
// RedisBus.replaySince applies knownFrom before delegating, and MemoryBus has
// only the guard below. Eviction and foreign-ID semantics are still the same
// in both.
func (rb *replayBuffer) since(sinceID int64) []Notification {
	// AN EMPTY BUFFER CANNOT PROVE A CLIENT IS CURRENT (BUG-2731, found by a
	// cross-artifact pass on that unit's branch). This buffer returning a
	// non-nil empty slice reads as "caught up" to both SSE handlers, and
	// MemoryBus assigns its own ids from 1 on every process start — so a
	// client resuming with a cursor from a previous incarnation was told it
	// had missed nothing.
	//
	// RedisBus never had this: replaySince applies the same rule through
	// knownFrom before delegating here. MemoryBus reached this function
	// directly, so the guard belongs at this level, where both get it.
	//
	// sinceID == 0 is exempt: a fresh subscriber is not resuming from a
	// position, so there is no span to vouch for.
	if rb.count == 0 {
		if sinceID > 0 {
			return nil
		}
		return []Notification{}
	}
	oldest := (rb.head - rb.count + rb.size) % rb.size
	oldestID := rb.items[oldest].ID
	newest := (rb.head - 1 + rb.size) % rb.size
	newestID := rb.items[newest].ID

	if sinceID > newestID {
		return nil
	}
	if sinceID > 0 && sinceID < oldestID && rb.count == rb.size {
		return nil
	}

	var result []Notification
	for i := 0; i < rb.count; i++ {
		idx := (oldest + i) % rb.size
		if rb.items[idx].ID > sinceID {
			result = append(result, rb.items[idx])
		}
	}
	if result == nil {
		result = []Notification{}
	}
	return result
}

// MemoryBus is an in-process implementation of Bus. See the package doc
// comment for its single-process limitation.
//
// A SINGLE mutex guards subscriber membership, sequence assignment, and
// the replay buffer together (codex round 1 findings 3 + 4 — the two
// were separate locks before, which allowed both out-of-ID-order buffer
// insertion under concurrent Publish AND a subscribe-then-replay window
// where a Notification could be delivered twice). This serializes
// Publish/Subscribe/EventsSince with respect to each other, which is the
// correct semantics for a monotonic sequence + ordered ring buffer
// anyway — genuinely concurrent publishes cannot both proceed and still
// yield a strictly-ordered buffer. Expected volume (status/assignment/
// comment notifications, not a firehose) makes the lack of reader
// concurrency a non-issue in practice.
//
// Publish also sends to every subscriber channel WHILE HOLDING mu
// (codex round 2 finding 3 — see Publish's doc comment for why an
// earlier "send after releasing the lock" version could panic).
type MemoryBus struct {
	// observable carries the optional operational-event seam (BUG-2727).
	//
	// cmd/pad/cmd_server.go wires one HERE TOO, not just on the Redis
	// bus, and that is deliberate: this bus has the same slow-subscriber
	// drop, so pad_watchevents_notifications_dropped_total means what it
	// says on a single-process deployment. The sequence-related reports
	// stay at zero here by construction — MemoryBus assigns its own
	// contiguous ids and has no subscription to lose.
	observable

	mu          sync.Mutex
	subscribers map[chan Notification]struct{}
	seq         int64
	replay      *replayBuffer
	// closed makes a post-Close Subscribe hand back an already-closed
	// channel rather than one nobody will ever close — see Subscribe.
	closed bool
}

// New creates a MemoryBus with the default replay buffer size.
func New() *MemoryBus {
	return NewWithReplaySize(DefaultReplayBufferSize)
}

// NewWithReplaySize creates a MemoryBus with a custom replay buffer
// capacity (tests use a small one).
func NewWithReplaySize(size int) *MemoryBus {
	return &MemoryBus{
		subscribers: make(map[chan Notification]struct{}),
		replay:      newReplayBuffer(size),
	}
}

// Publish assigns a sequence ID, appends to the replay buffer, and sends
// to every live subscriber — ALL under the same lock Unsubscribe/Close
// use to remove-and-close a channel (codex round 2 finding 3, fixing a
// send-on-closed-channel panic).
//
// The earlier version snapshotted subscriber channels under the lock,
// released it, and sent afterward — reasoning that holding the lock
// through a send would let a slow subscriber stall every other
// Publish/Subscribe/EventsSince call. That reasoning didn't hold up: the
// send below is already non-blocking (select/default — a full channel
// is dropped-and-logged, never awaited), so it can't stall anything
// regardless of the lock. What the released-lock version actually did
// was open a window between the snapshot and the send where
// Unsubscribe/Close could close a channel THIS call was about to send
// to — a send on a closed channel panics in Go, crashing the whole
// process (not a single subscriber's connection). Sending under the
// lock costs nothing (the send is O(1) and non-blocking either way) and
// closes that window structurally: Unsubscribe/Close can no longer run
// between "this channel is still a live subscriber" and "send to it".
func (b *MemoryBus) Publish(n Notification) error {
	if n.Timestamp == 0 {
		n.Timestamp = time.Now().UnixMilli()
	}

	// Registered FIRST so it runs LAST — after the Unlock below. See
	// Observer: reports fire with no bus lock held, so an observer may
	// call back into the bus without deadlocking it.
	var pending pendingReports
	defer func() { b.flush(&pending) }()

	b.mu.Lock()
	defer b.mu.Unlock()

	// BUG-2699: publishing after Close was the SILENT half of this bug,
	// and the worse half. Close empties b.subscribers, so the loop below
	// ran over an empty map and returned having done nothing at all — no
	// error (Publish had no way to say so), and unlike RedisBus not even
	// a log line, while still burning a sequence id and appending to a
	// replay buffer nobody will ever read from.
	//
	// The window is real for the SINGLE-PROCESS deployment, not only for
	// Redis: cmd/pad/cmd_server.go closes the watch bus BEFORE
	// srv.Shutdown drains handlers (deliberately — SSE handlers block on
	// their channel, so closing late holds shutdown to its full 30s
	// deadline on any open stream), and that ordering applies to
	// whichever implementation is wired. A push accepted moments earlier
	// can reach here post-Close, and used to be lost while the handler
	// answered 200.
	if b.closed {
		return ErrBusClosed
	}

	b.seq++
	n.ID = b.seq
	b.replay.append(n)

	for ch := range b.subscribers {
		select {
		case ch <- n:
		default:
			// A drop for SLOWNESS is not a Publish failure: the bus
			// accepted the notification and fanned it out, and one
			// subscriber's full buffer says nothing about the others.
			// Reporting it as an error would tell the push endpoint that
			// a delivery it cannot confirm either way had definitely
			// failed — exactly the false precision BUG-2699 is about.
			slog.Warn("watchevents: dropping notification for slow subscriber", "kind", n.Kind, "item_ref", n.ItemRef)
			pending.drop(DropReasonSlowSubscriber)
		}
	}
	return nil
}

func (b *MemoryBus) Subscribe() chan Notification {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Notification, 64)
	if b.closed {
		// Subscribing after Close used to register a channel that nobody
		// would ever close, so a consumer ranging over it blocked forever —
		// reachable during shutdown, where Stop closes the bus while a
		// stream handler is still running. RedisBus guarded this from the
		// start and the two implementations must not differ in a way a
		// consumer can feel (codex round 2 on BUG-2651).
		close(ch)
		return ch
	}
	b.subscribers[ch] = struct{}{}
	return ch
}

// SubscribeAndReplaySince — see the Bus interface doc comment for why
// this exists (codex round 1 finding 3). Subscribing and reading the
// replay buffer under the SAME critical section that Publish also uses
// means a Notification is either: published before this call (and thus
// only in the returned replay slice), or published after (and thus only
// delivered on the returned channel) — never both.
func (b *MemoryBus) SubscribeAndReplaySince(sinceID int64) (chan Notification, []Notification) {
	// Reported with the lock released, like every other report in this
	// package. RedisBus counts its own unservable resumes; MemoryBus did not,
	// so pad_watchevents_resume_gaps_total read zero on a single-process
	// deployment for a path that genuinely sends clients sync_required
	// (BUG-2731, codex round 12). A counter that is structurally blind to one
	// of its two implementations is worse than absent: it reads as evidence.
	var pending pendingReports
	defer func() { b.flush(&pending) }()

	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Notification, 64)
	if b.closed {
		// Same post-Close guard as Subscribe.
		close(ch)
		return ch, nil
	}
	b.subscribers[ch] = struct{}{}
	missed := b.replay.since(sinceID)
	if missed == nil {
		pending.resumeGap()
	}
	return ch, missed
}

func (b *MemoryBus) Unsubscribe(ch chan Notification) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subscribers[ch]; ok {
		delete(b.subscribers, ch)
		close(ch)
	}
}

func (b *MemoryBus) EventsSince(sinceID int64) []Notification {
	var pending pendingReports
	defer func() { b.flush(&pending) }()

	b.mu.Lock()
	defer b.mu.Unlock()
	missed := b.replay.since(sinceID)
	if missed == nil {
		pending.resumeGap()
	}
	return missed
}

func (b *MemoryBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subscribers {
		delete(b.subscribers, ch)
		close(ch)
	}
}
