package events

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/redisns"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// BUG-2736's Redis half. The counter is shared ACROSS processes, so no single
// instance can compute which incarnation of it an ID belongs to — the way
// MemoryBus does with its own base. The identity therefore travels WITH each
// message, as an opaque epoch in a "<epoch>|<id>|<json>" prefix, and emitting
// it is gated behind a two-phase flip because an older binary cannot parse it.

// listen subscribes to a channel with a REAL go-redis client and returns a
// function that reads the next published payload. Using the real client rather
// than poking miniredis keeps the bytes under test the bytes a receiving
// instance would actually see.
func listen(t *testing.T, client *redis.Client, channel string) func() string {
	t.Helper()
	ps := client.Subscribe(context.Background(), channel)
	t.Cleanup(func() { _ = ps.Close() })
	if _, err := ps.Receive(context.Background()); err != nil {
		t.Fatalf("subscribe to %s: %v", channel, err)
	}
	ch := ps.Channel()
	return func() string {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatal("subscription closed before a message arrived")
			}
			return msg.Payload
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a published message")
			return ""
		}
	}
}

func newFlippedRedisBus(t *testing.T) (*RedisBus, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBusWithKeys(client, redisns.Default, true)
	t.Cleanup(b.Close)
	return b, mr
}

// --- the wire form ------------------------------------------------------

func TestDecodePayloadAcceptsBothWireForms(t *testing.T) {
	// Both directions of the roll depend on this function, so every branch is
	// pinned rather than the happy one.
	bare, err := json.Marshal(Event{ID: 42, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	t.Run("prefixed", func(t *testing.T) {
		body, _ := json.Marshal(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
		epoch, ev, err := decodePayload("7|77|" + string(body))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if epoch != 7 {
			t.Fatalf("epoch: want 7, got %d", epoch)
		}
		// The ID comes from the PREFIX, not the body — the body was
		// marshalled before the ID existed.
		if ev.ID != 77 {
			t.Fatalf("id: want 77, got %d", ev.ID)
		}
		if ev.WorkspaceID != "ws-1" {
			t.Fatalf("workspace: want ws-1, got %q", ev.WorkspaceID)
		}
	})

	t.Run("bare, which is what every phase-1 publisher emits", func(t *testing.T) {
		epoch, ev, err := decodePayload(string(bare))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if epoch != 0 {
			t.Fatalf("a bare payload carries no id-space information; got epoch %d", epoch)
		}
		if ev.ID != 42 {
			t.Fatalf("id: want 42 from the body, got %d", ev.ID)
		}
	})

	t.Run("a bare body containing pipes is not mistaken for a prefix", func(t *testing.T) {
		// The discriminating case for the leading-'{' check. Without it this
		// JSON splits into three parts and is rejected as a bad prefix.
		body, _ := json.Marshal(Event{ID: 9, Type: ItemUpdated, WorkspaceID: "ws-1", Title: "a|b|c"})
		if !strings.Contains(string(body), "|") {
			t.Fatal("fixture: the body must contain pipes for this case to mean anything")
		}
		epoch, ev, err := decodePayload(string(body))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if epoch != 0 || ev.ID != 9 || ev.Title != "a|b|c" {
			t.Fatalf("want bare decode with title intact, got epoch=%d id=%d title=%q", epoch, ev.ID, ev.Title)
		}
	})

	t.Run("rejections", func(t *testing.T) {
		body, _ := json.Marshal(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
		for name, payload := range map[string]string{
			"empty epoch":         "|77|" + string(body),
			"non-integer epoch":   "e-1|77|" + string(body),
			"zero epoch sentinel": "0|77|" + string(body),
			"negative epoch":      "-3|77|" + string(body),
			"non-integer id":      "7|seventy|" + string(body),
			"zero id":             "7|0|" + string(body),
			"negative id":         "7|-4|" + string(body),
			"body is not JSON":    "7|77|not json",
			"neither form":        "not json at all",
			// THE BARE FORM MUST BE HELD TO THE SAME RULE (codex round 16).
			// The id check lived in the prefixed branch only, so these were
			// accepted: delivered with no SSE cursor, and once an epoch is
			// adopted, read as the sequence going backwards and used to
			// discard every replay buffer.
			"bare, zero id":     string(mustMarshalEvent(t, Event{ID: 0, Type: ItemUpdated, WorkspaceID: "ws-1"})),
			"bare, negative id": string(mustMarshalEvent(t, Event{ID: -7, Type: ItemUpdated, WorkspaceID: "ws-1"})),
			"prefix without id": "7|" + string(body),
		} {
			if _, _, err := decodePayload(payload); err == nil {
				t.Errorf("%s: want an error, got none", name)
			}
		}
	})
}

func TestPhaseTwoPublishesThePrefixedFormAndPhaseOneDoesNot(t *testing.T) {
	// The flip's ONLY observable difference is the bytes on the wire, so this
	// reads them rather than any internal flag.
	t.Run("flipped", func(t *testing.T) {
		b, _ := newFlippedRedisBus(t)
		next := listen(t, b.client, redisns.Default.Name(redisChannelSuffix)+"ws-1")

		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "item-7", Title: "carried"})

		epoch, ev, err := decodePayload(next())
		if err != nil {
			t.Fatalf("a flipped instance must emit a decodable payload: %v", err)
		}
		if epoch <= 0 {
			t.Fatalf("a flipped instance must emit a positive epoch generation, got %d", epoch)
		}
		if ev.ID != 1 {
			t.Fatalf("first id from a fresh counter must be 1, got %d", ev.ID)
		}
		// THE BODY MUST SURVIVE THE PREFIX. Without this, `1|1|{}` satisfies
		// every assertion above while the event's fields are lost — a wire
		// format that is well-formed and empty.
		if ev.Type != ItemUpdated || ev.WorkspaceID != "ws-1" || ev.ItemID != "item-7" || ev.Title != "carried" {
			t.Fatalf("the event body must survive the prefix, got %+v", ev)
		}
	})

	t.Run("not flipped", func(t *testing.T) {
		mr := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		b := NewRedisBusWithKeys(client, redisns.Default, false)
		t.Cleanup(b.Close)

		next := listen(t, client, redisns.Default.Name(redisChannelSuffix)+"ws-1")

		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

		// A pre-phase-1 binary must be able to json.Unmarshal this directly —
		// that is the entire compatibility claim, so assert it the way that
		// binary would rather than through decodePayload, which accepts both.
		var ev Event
		if err := json.Unmarshal([]byte(next()), &ev); err != nil {
			t.Fatalf("an un-flipped instance must emit bare JSON an older binary can parse: %v", err)
		}
		if ev.ID != 1 {
			t.Fatalf("the bare form carries the id INSIDE the body; want 1, got %d", ev.ID)
		}
		if mr.Exists(redisns.Default.Name(redisEpochSuffix)) {
			t.Fatal("an un-flipped instance must not write the epoch key")
		}
	})
}

func TestTheDedupeTokenMakesARetriedPublishANoOp(t *testing.T) {
	// go-redis retries a command whose REPLY was lost, so the script can run,
	// publish, and still return an error to its caller. Driving the script
	// twice with the same token is that retry.
	b, mr := newFlippedRedisBus(t)
	channel := redisns.Default.Name(redisChannelSuffix) + "ws-1"
	ps := b.client.Subscribe(context.Background(), channel)
	defer func() { _ = ps.Close() }()
	if _, err := ps.Receive(context.Background()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	incoming := ps.Channel()

	body, _ := json.Marshal(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	keys := []string{
		redisns.Default.Name(redisSeqSuffix),
		channel,
		redisns.Default.Name(redisEpochSuffix),
		redisns.Default.Name(redisDedupeSuffix) + "fixed-token",
		redisns.Default.Name(redisEpochGenSuffix),
	}

	first, err := publishScript.Run(b.ctx, b.client, keys, string(body), redisDedupeTTLSeconds).Int64()
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first != 1 {
		t.Fatalf("first run must assign id 1, got %d", first)
	}

	second, err := publishScript.Run(b.ctx, b.client, keys, string(body), redisDedupeTTLSeconds).Int64()
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second != 0 {
		t.Fatalf("a retry carrying the same token must decline, got id %d", second)
	}
	select {
	case <-incoming:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first publish")
	}
	select {
	case msg := <-incoming:
		t.Fatalf("a retried publish must reach the channel once; a second message arrived: %q", msg.Payload)
	case <-time.After(150 * time.Millisecond):
	}
	// And it must not have burned an id either, or the sequence develops holes
	// that look like lost events to every receiver.
	got, err := mr.Get(redisns.Default.Name(redisSeqSuffix))
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if got != "1" {
		t.Fatalf("a declined retry must not INCR the counter; counter reads %q", got)
	}
}

// --- reconciliation on the receive path ---------------------------------

// liveGen subscribes the workspace and returns the generation a real receive
// loop would carry, so these tests drive fanOut the way a message does rather
// than through the anySubscription escape hatch.
func liveGen(t *testing.T, b *RedisBus, workspaceID string) (chan Event, int64) {
	t.Helper()
	ch, _ := b.Subscribe(workspaceID)
	t.Cleanup(func() { b.Unsubscribe(ch) })
	return ch, b.currentSubGen(workspaceID)
}

func TestAdoptingAnEpochOntoEmptyBuffersIsNotAReset(t *testing.T) {
	// Deliberate: otherwise every instance reports a reset at startup and
	// pad_event_sequence_resets_total grows a per-deploy baseline, which is
	// the thing that makes the counter unreadable.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)

	_, gen := liveGen(t, b, "ws-1")
	b.fanOutFromRedis(gen, 1, Event{ID: 10, Type: ItemUpdated, WorkspaceID: "ws-1"})

	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("learning an epoch on an empty bus must not report a reset, got %v", resets)
	}
	// And coverage is established from that first id.
	if got := b.EventsSince("ws-1", 10); got == nil {
		t.Fatal("the adopting message must establish coverage")
	}
}

func TestAdoptingAnEpochOntoBufferedEventsIsAReset(t *testing.T) {
	// The phase-2 roll's dangerous shape: this instance buffered BARE events
	// from a phase-1 publisher, the counter was then reset, and a flipped
	// publisher's first prefixed message arrives with an id ABOVE our
	// high-water mark. The numeric check sees an ordinary successor; only the
	// epoch says the space changed.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)

	_, gen := liveGen(t, b, "ws-1")
	b.fanOutFromRedis(gen, 0, Event{ID: 5, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if got := b.EventsSince("ws-1", 5); got == nil {
		t.Fatal("fixture: the bare event must establish coverage first")
	}

	b.fanOutFromRedis(gen, 1, Event{ID: 6, Type: ItemUpdated, WorkspaceID: "ws-1"})

	_, resets := obs.snapshot()
	if len(resets) != 1 || resets[0] != ResetReasonEpochChange {
		t.Fatalf("want one %s, got %v", ResetReasonEpochChange, resets)
	}
	// The pre-reset cursor must now be refused: 6 did NOT follow 5.
	if got := b.EventsSince("ws-1", 5); got != nil {
		t.Fatalf("a cursor from the discarded space must be a gap, got %d events", len(got))
	}
	// Control: a cursor at the new space's first id is served.
	if got := b.EventsSince("ws-1", 6); got == nil {
		t.Fatal("a cursor at the new space's first id must be served")
	}

	// ONE DROP PER INSTANCE PER ROLL, asserted rather than claimed in prose.
	// The adoption comment says the cost is bounded at one; further messages
	// in the same generation must leave the buffers alone, or the "bounded"
	// in that sentence is doing no work.
	b.fanOutFromRedis(gen, 1, Event{ID: 7, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 1, Event{ID: 8, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if _, resets := obs.snapshot(); len(resets) != 1 {
		t.Fatalf("later messages in the same generation must not drop again; resets are now %v", resets)
	}
	if got := b.EventsSince("ws-1", 6); got == nil || len(got) != 2 {
		t.Fatalf("coverage must extend across the rest of the generation, got %v", got)
	}
}

func TestAnEpochChangeDropsEveryWorkspaceAndClearsTheFloor(t *testing.T) {
	// The counter is global, so a reset invalidates every workspace at once —
	// and the floor is a SAME-SPACE device, so it must not survive into a
	// space where its numbers mean nothing (that combination is a resync loop).
	b := newTestRedisBus(t)
	_, gen1 := liveGen(t, b, "ws-1")
	_, gen2 := liveGen(t, b, "ws-2")

	b.fanOutFromRedis(gen1, 1, Event{ID: 100, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen2, 1, Event{ID: 101, Type: ItemUpdated, WorkspaceID: "ws-2"})

	// A backwards id inside epoch-a raises the floor to 101.
	b.fanOutFromRedis(gen1, 1, Event{ID: 50, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if b.discardedHighWater != 101 {
		t.Fatalf("fixture: floor should stand at 101, got %d", b.discardedHighWater)
	}

	// Now the space itself changes.
	b.fanOutFromRedis(gen1, 2, Event{ID: 1, Type: ItemUpdated, WorkspaceID: "ws-1"})

	if b.discardedHighWater != 0 {
		t.Fatalf("an epoch change must clear the floor, got %d", b.discardedHighWater)
	}
	if _, ok := b.replayBuffers["ws-2"]; ok {
		t.Fatal("an epoch change must drop every workspace's buffer, not just the arriving one")
	}
	// The new space's first id is servable — which it would NOT be if the
	// floor had survived, because 1 is far below 101. That is the resync loop
	// this clears.
	if got := b.EventsSince("ws-1", 1); got == nil {
		t.Fatal("the new space's first id must be servable; a surviving floor is a resync loop")
	}
}

func TestACounterGoingBackwardsRaisesTheFloor(t *testing.T) {
	// The mixed-VERSION ordering case DURING THE PHASE-2 ROLL: an un-flipped
	// publisher assigns and publishes in two calls, so a lower id can land
	// after a higher one. The events between the two are unrecoverable here,
	// so every cursor at or below what we discarded is refused.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	_, gen := liveGen(t, b, "ws-1")

	// An epoch must be adopted first: the check is gated on it, because on a
	// pure phase-1 deployment a backwards id is ordinary interleave rather
	// than evidence of anything. The negative control below pins that gate.
	b.fanOutFromRedis(gen, 3, Event{ID: 200, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 0, Event{ID: 150, Type: ItemUpdated, WorkspaceID: "ws-1"})

	_, resets := obs.snapshot()
	if len(resets) != 1 || resets[0] != ResetReasonCounterBackward {
		t.Fatalf("want one %s, got %v", ResetReasonCounterBackward, resets)
	}
	for _, cursor := range []int64{149, 150, 199, 200} {
		if got := b.EventsSince("ws-1", cursor); got != nil {
			t.Fatalf("cursor %d is at or below the discarded high-water mark 200 and must be a gap, got %d events", cursor, len(got))
		}
	}
	// Control: once the sequence climbs past the discarded mark, resumes work
	// again — the refusal is bounded, not permanent.
	b.fanOutFromRedis(gen, 0, Event{ID: 201, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if got := b.EventsSince("ws-1", 201); got == nil {
		t.Fatal("a cursor above the discarded high-water mark must be served")
	}
}

func TestAPhaseOneInterleaveDoesNotDropEveryBuffer(t *testing.T) {
	// codex round 9, and the reason the backwards check is gated on having
	// adopted an epoch. Phase 1 publishes with a two-call INCR-then-PUBLISH,
	// so on any multi-instance deployment two publishers interleave routinely
	// and a lower id arrives after a higher one as ORDINARY TRAFFIC.
	//
	// Reacting to that would drop every replay buffer and resync every client
	// — in the DEFAULT configuration, which is where every deployment sits
	// until an operator flips phase 2. This diff would have introduced that
	// regression; the gate is what prevents it, and this is the test that
	// fails if the gate is removed.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	_, gen := liveGen(t, b, "ws-1")
	_, gen2 := liveGen(t, b, "ws-2")

	// PREMISE: no epoch has been adopted, which is what a phase-1 deployment
	// looks like from here. If this ever stops holding, the assertions below
	// are about a different situation.
	if b.epoch != 0 {
		t.Fatalf("fixture: a phase-1 bus must hold no epoch, got %d", b.epoch)
	}

	b.fanOutFromRedis(gen, 0, Event{ID: 200, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen2, 0, Event{ID: 201, Type: ItemUpdated, WorkspaceID: "ws-2"})
	// The interleave: 199 was assigned before 200 and published after it.
	b.fanOutFromRedis(gen, 0, Event{ID: 199, Type: ItemUpdated, WorkspaceID: "ws-1"})

	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("an ordinary phase-1 interleave must not be reported as a sequence reset, got %v", resets)
	}
	// The OTHER workspace's coverage must survive: this branch drops every
	// buffer, so a regression here is charged to clients whose stream never
	// went near the interleave.
	if got := b.EventsSince("ws-2", 201); got == nil {
		t.Fatal("an unrelated workspace's coverage must survive a phase-1 interleave")
	}

	// WHAT IS NOT CLAIMED, so this test is not read as promising more than it
	// does: the INTERLEAVED workspace's own buffer now holds ids out of order,
	// and replayBuffer.since computes newest by POSITION — so a cursor at the
	// higher id looks foreign and is refused, while one at the lower id is
	// served. That is pre-existing behaviour, unchanged by this diff (main has
	// the same buffer and no backwards handling at all), and it is strictly
	// less harmful than dropping every workspace's buffer. Asserted rather
	// than described, so a future change to since() surfaces here.
	if got := b.EventsSince("ws-1", 200); got != nil {
		t.Fatalf("expected the out-of-order cursor to read as foreign (pre-existing), got %d events", len(got))
	}
	if got := b.EventsSince("ws-1", 199); got == nil {
		t.Fatal("a cursor at the buffer's last-appended id must still be served")
	}
}

func TestABarePayloadAfterAdoptionLeavesTheEpochAlone(t *testing.T) {
	// Silence is not evidence of a change. During the phase-2 roll a flipped
	// and an un-flipped publisher both feed this instance; if the bare form
	// were read as "epoch changed to empty", every alternating message would
	// drop every buffer.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	_, gen := liveGen(t, b, "ws-1")

	b.fanOutFromRedis(gen, 1, Event{ID: 10, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 0, Event{ID: 11, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 1, Event{ID: 12, Type: ItemUpdated, WorkspaceID: "ws-1"})

	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("a mixed-format stream inside one epoch must report no resets, got %v", resets)
	}
	if got := b.EventsSince("ws-1", 10); got == nil || len(got) != 2 {
		t.Fatalf("coverage must span the mixed-format run, got %v", got)
	}
	if b.epoch != 1 {
		t.Fatalf("the adopted epoch must survive a bare message, got %d", b.epoch)
	}
}

func TestAPhaseOneRestartClearsAStaleEpoch(t *testing.T) {
	// codex round 2. The sequence of events that made this reachable:
	// phase 2 mints an epoch and the counter climbs; the deployment rolls
	// back to phase 1; the seq key is lost; phase-1 publishers climb from 1
	// again; phase 2 is re-enabled and finds the OLD epoch still there, so
	// nothing reports a change and two spaces can merge in one buffer.
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	epochKey := redisns.Default.Name(redisEpochSuffix)
	seqKey := redisns.Default.Name(redisSeqSuffix)

	// Phase 2 first: it is what writes an epoch at all.
	flipped := NewRedisBusWithKeys(client, redisns.Default, true)
	t.Cleanup(flipped.Close)
	flipped.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

	stale, err := mr.Get(epochKey)
	if err != nil || stale == "" {
		t.Fatalf("fixture: phase 2 must have written an epoch, got %q (%v)", stale, err)
	}

	// Roll back to phase 1, then lose the counter.
	phase1 := NewRedisBusWithKeys(client, redisns.Default, false)
	t.Cleanup(phase1.Close)
	mr.Del(seqKey)

	phase1.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

	// PREMISE: the counter really did restart, or the branch under test was
	// never entered and the assertion below would be about nothing.
	if got, err := mr.Get(seqKey); err != nil || got != "1" {
		t.Fatalf("fixture: the counter should have restarted at 1, reads %q (%v)", got, err)
	}
	if mr.Exists(epochKey) {
		t.Fatal("a restarted counter must clear the stale epoch, or a later phase-2 publish reuses it for a space that no longer exists")
	}

	// Control: an ordinary phase-1 publish, with the counter simply climbing,
	// must NOT touch the epoch — otherwise every publish rotates it and the
	// mechanism is worthless.
	flipped.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	fresh, err := mr.Get(epochKey)
	if err != nil || fresh == "" {
		t.Fatalf("fixture: the re-flipped publish must mint a new epoch, got %q (%v)", fresh, err)
	}
	if fresh == stale {
		t.Fatal("the new epoch must differ from the one the dead space used")
	}
	phase1.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	after, err := mr.Get(epochKey)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if after != fresh {
		t.Fatalf("a phase-1 publish on a climbing counter must leave the epoch alone; %q became %q", fresh, after)
	}
}

func TestAStragglerFromAnAbandonedIDSpaceIsDiscarded(t *testing.T) {
	// codex round 3. Each workspace has its own subscription and its own
	// receive goroutine, and Redis orders messages within a channel but not
	// ACROSS them — so a message published before a rotation, on workspace A's
	// channel, can arrive after the rotation was learned from workspace B's.
	//
	// With an unordered epoch this was indistinguishable from a genuine
	// rotation: the bus flipped BACK to the dead generation, dropped every
	// buffer a second time, and would have appended a dead-space id into the
	// fresh buffer.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	_, genA := liveGen(t, b, "ws-a")
	_, genB := liveGen(t, b, "ws-b")

	// Generation 1 is learned and buffered.
	b.fanOutFromRedis(genA, 1, Event{ID: 500, Type: ItemUpdated, WorkspaceID: "ws-a"})
	// Generation 2 arrives on the other workspace: a genuine rotation.
	b.fanOutFromRedis(genB, 2, Event{ID: 1, Type: ItemUpdated, WorkspaceID: "ws-b"})

	_, resets := obs.snapshot()
	if len(resets) != 1 || resets[0] != ResetReasonEpochChange {
		t.Fatalf("fixture: the rotation must report exactly one %s, got %v", ResetReasonEpochChange, resets)
	}
	if b.epoch != 2 {
		t.Fatalf("fixture: the bus should have adopted generation 2, got %d", b.epoch)
	}

	// NOW the straggler: published under generation 1, delivered late.
	b.fanOutFromRedis(genA, 1, Event{ID: 501, Type: ItemUpdated, WorkspaceID: "ws-a"})

	if b.epoch != 2 {
		t.Fatalf("a straggler from an abandoned generation must not move the epoch; it became %d", b.epoch)
	}
	// It must not be BUFFERED, or the dead space's id sits in a live buffer
	// and a resume can be answered across the boundary.
	if rb, ok := b.replayBuffers["ws-a"]; ok && rb.count != 0 {
		t.Fatalf("the straggler must not be appended; ws-a's buffer holds %d events", rb.count)
	}
	// AND IT ENDS COVERAGE (codex round 19). Discarding the message while
	// leaving the buffers valid told a reconnecting client it was caught up —
	// harmless if this really is a straggler, and thirty seconds of silently
	// missed events if the generation regressed instead. The bus cannot tell
	// which at this moment, which is exactly why it must not claim to.
	_, resets = obs.snapshot()
	if len(resets) != 2 || resets[1] != ResetReasonEpochRegressed {
		t.Fatalf("a lower generation must end coverage and be reported as %s, got %v", ResetReasonEpochRegressed, resets)
	}
	if got := b.EventsSince("ws-b", 1); got != nil {
		t.Fatalf("coverage must have ended while the generation is in doubt, got %d events", len(got))
	}

	// Control: the live generation re-establishes coverage immediately, so the
	// end of coverage is a resync and not a dead bus.
	b.fanOutFromRedis(genA, 2, Event{ID: 2, Type: ItemUpdated, WorkspaceID: "ws-a"})
	if got := b.EventsSince("ws-a", 2); got == nil {
		t.Fatal("a message in the live generation must establish coverage again")
	}
}

func TestEpochGenerationsAreMintedByRedisAndIncrease(t *testing.T) {
	// The property the whole ordering rule rests on: a rotation's generation
	// is always higher than the one it replaces, and it comes from Redis
	// rather than from any instance's clock or uuid. Driven through two real
	// resets rather than by poking the key.
	b, mr := newFlippedRedisBus(t)
	epochKey := redisns.Default.Name(redisEpochSuffix)
	seqKey := redisns.Default.Name(redisSeqSuffix)

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	first, err := mr.Get(epochKey)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}

	mr.Del(seqKey)
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	second, err := mr.Get(epochKey)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}

	mr.Del(seqKey)
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	third, err := mr.Get(epochKey)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}

	g1, g2, g3 := mustParseGeneration(t, first), mustParseGeneration(t, second), mustParseGeneration(t, third)
	if !(g1 < g2 && g2 < g3) {
		t.Fatalf("generations must strictly increase across resets, got %d, %d, %d", g1, g2, g3)
	}
	if g1 <= 0 {
		t.Fatalf("a generation must be positive (zero is the no-information sentinel), got %d", g1)
	}
}

func mustMarshalEvent(t *testing.T, e Event) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustParseGeneration(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("epoch %q is not an integer generation: %v", s, err)
	}
	return n
}

func TestAPrefixedPayloadReachesReconciliationThroughTheRealReceivePath(t *testing.T) {
	// codex round 4. Every other test in this file drives fanOutFromRedis
	// directly, and the publish tests read the wire with a raw subscriber —
	// so a regression that decoded the epoch correctly and then handed 0 to
	// the fan-out would pass all of them, and reconciliation would silently
	// never run in production.
	b, mr := newFlippedRedisBus(t)

	ch, _ := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)
	// Subscribe() returns before Redis has REGISTERED the subscription. It
	// does write the SUBSCRIBE command synchronously, but it does not wait
	// for the server to confirm it (go-redis PubSub.subscribe writes and
	// returns), and the publish that follows travels on a DIFFERENT
	// connection — so Redis can process the publish first. Publishing into
	// that window loses the event OUTRIGHT, not slowly: RedisBus.Publish has
	// no local fan-out, and nothing replays a pub/sub message nobody was
	// listening for. Hence a wait rather than a longer timeout — the round
	// trip here is bimodal, tens of milliseconds or never (BUG-2742).
	waitForSubscribers(t, mr, redisns.Default.Name(redisChannelSuffix)+"ws-1", true)

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "item-7"})

	var received Event
	select {
	case received = <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("the published event never came back through Redis")
	}

	// The id came from the PREFIX, not the body — the body was marshalled
	// before the id existed, so a receive path that ignored the prefix would
	// deliver 0 here.
	if received.ID != 1 {
		t.Fatalf("the id must come from the prefix; got %d", received.ID)
	}
	if received.ItemID != "item-7" {
		t.Fatalf("the body must survive the round trip, got %+v", received)
	}

	b.mu.Lock()
	adopted := b.epoch
	b.mu.Unlock()
	if adopted <= 0 {
		t.Fatalf("the epoch must reach the bus through the receive path; it holds %d", adopted)
	}

	// And the event is buffered under that id, so a resume can be answered.
	if got := b.EventsSince("ws-1", received.ID); got == nil {
		t.Fatal("the received event must establish coverage")
	}
}

func TestConcurrentPhaseTwoPublishesArriveInIDOrder(t *testing.T) {
	// codex round 4. The atomic script's whole justification is that publish
	// order equals ID order globally — which matters because the receive path
	// reads a descending ID as a counter reset and drops every buffer. Every
	// other phase-2 test publishes once or drives the script sequentially, so
	// a two-call INCR-then-PUBLISH implementation passes all of them.
	//
	// The instrument: many concurrent publishes, read off the channel in
	// arrival order. Under the script, arrival order IS ID order. Under two
	// calls, two publishers interleave between their INCR and their PUBLISH
	// and a lower ID arrives after a higher one.
	b, _ := newFlippedRedisBus(t)
	channel := redisns.Default.Name(redisChannelSuffix) + "ws-1"

	ps := b.client.Subscribe(context.Background(), channel)
	defer func() { _ = ps.Close() }()
	if _, err := ps.Receive(context.Background()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	incoming := ps.Channel()

	const n = 300
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
		}()
	}
	wg.Wait()

	var prev int64
	for i := 0; i < n; i++ {
		select {
		case msg := <-incoming:
			_, ev, err := decodePayload(msg.Payload)
			if err != nil {
				t.Fatalf("message %d: %v", i, err)
			}
			if ev.ID <= prev {
				t.Fatalf("message %d arrived out of ID order: %d after %d — publish order must equal ID order", i, ev.ID, prev)
			}
			prev = ev.ID
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d messages arrived", i, n)
		}
	}
	// PREMISE: all n really were published, so the ordering assertion ran over
	// the whole set rather than over a handful that happened to be ordered.
	if prev != n {
		t.Fatalf("the last id should be %d after %d publishes, got %d", n, n, prev)
	}
}

func TestAPersistentlyRegressedGenerationIsAcceptedAsANewSpace(t *testing.T) {
	// codex round 6, scenario (b). A Redis failover to a replica whose copy of
	// the generation counter predates the last rotation makes every publisher
	// mint from a LOWER number — and the straggler rule would then discard
	// every message forever: nothing delivered, nothing buffered, and the only
	// trace a log line per message. Silent and unbounded is the one outcome
	// this family refuses.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	_, gen := liveGen(t, b, "ws-1")

	b.fanOutFromRedis(gen, 5, Event{ID: 900, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if b.epoch != 5 {
		t.Fatalf("fixture: generation 5 should have been adopted, got %d", b.epoch)
	}

	// PREMISE: inside the straggler window a lower generation is DISCARDED.
	// Asserting it here is what makes the second half meaningful — otherwise
	// this test would pass on a bus with no straggler rule at all.
	b.fanOutFromRedis(gen, 4, Event{ID: 10, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if b.epoch != 5 {
		t.Fatalf("inside the window a lower generation must be discarded; the epoch became %d", b.epoch)
	}

	// Now the same generation arrives long after the adoption, which no
	// in-flight message can do.
	b.mu.Lock()
	b.epochAdoptedAt = time.Now().Add(-2 * stragglerWindow)
	b.mu.Unlock()

	b.fanOutFromRedis(gen, 4, Event{ID: 11, Type: ItemUpdated, WorkspaceID: "ws-1"})

	if b.epoch != 4 {
		t.Fatalf("a persistent regression must be accepted as a new space; the epoch is %d", b.epoch)
	}
	_, resets := obs.snapshot()
	if len(resets) == 0 || resets[len(resets)-1] != ResetReasonEpochRegressed {
		t.Fatalf("the regression must be reported as %s, got %v", ResetReasonEpochRegressed, resets)
	}
	// And delivery RESUMES: the regressed space's next message is buffered, so
	// clients are served again rather than starved.
	b.fanOutFromRedis(gen, 4, Event{ID: 12, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if got := b.EventsSince("ws-1", 12); got == nil {
		t.Fatal("after accepting the regression the bus must serve the new space again")
	}
}

func TestACorruptedEpochKeyIsRotatedRatherThanEmitted(t *testing.T) {
	// codex round 11. A publisher trusted whatever the epoch key held. Set to
	// something that is not a positive generation — corrupted, hand-edited, or
	// written by another installation sharing the keyspace — it would be
	// emitted into every prefix, every receiver would reject the payload, and
	// every event would be dropped for as long as the key stayed that way.
	b, mr := newFlippedRedisBus(t)
	epochKey := redisns.Default.Name(redisEpochSuffix)
	next := listen(t, b.client, redisns.Default.Name(redisChannelSuffix)+"ws-1")

	// The last two are the same defect by a different route (codex round 13):
	// all digits, so a pattern match alone accepts them, and both overflow the
	// int64 the RECEIVER parses them into — so the payload would be rejected
	// on arrival and every event dropped, exactly as with "abc".
	for _, corrupt := range []string{"abc", "0", "-3", "1.5", "", "9223372036854775808", "99999999999999999999999999"} {
		if err := mr.Set(epochKey, corrupt); err != nil {
			t.Fatalf("seed epoch %q: %v", corrupt, err)
		}
		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

		epoch, _, err := decodePayload(next())
		if err != nil {
			t.Fatalf("epoch %q: the publisher must emit a decodable payload, got %v", corrupt, err)
		}
		if epoch <= 0 {
			t.Fatalf("epoch %q: want a positive generation on the wire, got %d", corrupt, epoch)
		}
		stored, err := mr.Get(epochKey)
		if err != nil || stored == corrupt {
			t.Fatalf("epoch %q: the key must be rotated, it reads %q (%v)", corrupt, stored, err)
		}
	}

	// Control: a VALID epoch is left alone, or the rotation fires on every
	// publish and the generation means nothing.
	valid, err := mr.Get(epochKey)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	_, _, _ = decodePayload(next())
	after, err := mr.Get(epochKey)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if after != valid {
		t.Fatalf("a valid epoch must survive a publish; %q became %q", valid, after)
	}
}

func TestAnUndecodableMessageEndsThatWorkspacesCoverage(t *testing.T) {
	// codex round 11. Dropping an unreadable message and carrying on left the
	// buffer claiming a span with a hole in it: the event gone, the ids either
	// side contiguous, and a later resume across it answered "caught up".
	b, mr := newFlippedRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)

	ch, _ := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)
	// See TestAPrefixedPayloadReachesReconciliationThroughTheRealReceivePath:
	// publishing before the subscription is registered loses the event for
	// good, and here it would look like the coverage-establishing publish
	// simply never arrived (BUG-2742).
	waitForSubscribers(t, mr, redisns.Default.Name(redisChannelSuffix)+"ws-1", true)
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

	var first Event
	select {
	case first = <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("the first event never arrived")
	}
	// PREMISE: coverage exists before the garbage arrives, or the assertion
	// below is about a bus that never had any.
	if got := b.EventsSince("ws-1", first.ID); got == nil {
		t.Fatal("fixture: coverage must be established first")
	}

	// Something publishes onto our channel that we cannot read.
	if err := b.client.Publish(context.Background(),
		redisns.Default.Name(redisChannelSuffix)+"ws-1", "not a payload").Err(); err != nil {
		t.Fatalf("publish garbage: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b.EventsSince("ws-1", first.ID) == nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := b.EventsSince("ws-1", first.ID); got != nil {
		t.Fatalf("a message we could not read is a hole; the resume must be a gap, got %d events", len(got))
	}
	_, resets := obs.snapshot()
	if len(resets) == 0 || resets[len(resets)-1] != ResetReasonUndecodableMessage {
		t.Fatalf("want the drop reported as %s, got %v", ResetReasonUndecodableMessage, resets)
	}
}

func TestAScriptThatDiedBeforePublishingDoesNotBlockItsRetry(t *testing.T) {
	// codex round 11, and the reason the dedupe token is CHECKED first and
	// WRITTEN last. Redis runs Lua atomically against interleaving, not with
	// rollback: a script that errors part way through keeps whatever it
	// already wrote. Written first, the token would survive a run that never
	// published, and the retry would decline — the event lost permanently and
	// silently, with the caller told it succeeded.
	b, mr := newFlippedRedisBus(t)
	channel := redisns.Default.Name(redisChannelSuffix) + "ws-1"
	next := listen(t, b.client, channel)

	body, _ := json.Marshal(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	keys := []string{
		redisns.Default.Name(redisSeqSuffix), channel,
		redisns.Default.Name(redisEpochSuffix),
		redisns.Default.Name(redisDedupeSuffix) + "one-logical-publish",
		redisns.Default.Name(redisEpochGenSuffix),
	}

	// Make INCR fail: a sequence key holding something that is not an integer.
	// This stands in for any mid-script error — a wrong-typed key, an ACL
	// denial — since the script's failure MODE is what matters, not its cause.
	if err := mr.Set(redisns.Default.Name(redisSeqSuffix), "not-a-number"); err != nil {
		t.Fatalf("seed sequence: %v", err)
	}
	if err := publishScript.Run(b.ctx, b.client, keys, string(body), redisDedupeTTLSeconds).Err(); err == nil {
		t.Fatal("fixture: the script must fail here, or this test proves nothing")
	}

	// The obstacle clears — the operator fixes the key, the ACL is restored —
	// and go-redis retries the same logical publish with the SAME token.
	mr.Del(redisns.Default.Name(redisSeqSuffix))
	id, err := publishScript.Run(b.ctx, b.client, keys, string(body), redisDedupeTTLSeconds).Int64()
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if id == 0 {
		t.Fatal("a retry of a publish that never happened must not be declined; the event would be lost with the caller told it succeeded")
	}
	if payload := next(); payload == "" {
		t.Fatal("the retry must actually publish")
	}
}

func TestAMixedPhaseDeploymentDeliversBothWays(t *testing.T) {
	// codex round 12. Parsing, publishing and fan-out were each tested alone;
	// the CLAIM the two phases rest on is that a phase-1 and a phase-2
	// instance on one Redis deliver each other's events. That needs both buses
	// at once, which nothing exercised.
	mr := miniredis.RunT(t)
	newBus := func(publishEpoch bool) *RedisBus {
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		b := NewRedisBusWithKeys(client, redisns.Default, publishEpoch)
		t.Cleanup(b.Close)
		return b
	}
	phase1, phase2 := newBus(false), newBus(true)

	// Both are subscribed to the same workspace, as two replicas serving
	// clients would be.
	ch1, _ := phase1.Subscribe("ws-1")
	defer phase1.Unsubscribe(ch1)
	ch2, _ := phase2.Subscribe("ws-1")
	defer phase2.Unsubscribe(ch2)
	// BOTH must be registered before anything is published: this test's whole
	// claim is that each replica sees the other's events, and a publish that
	// lands while only one of them has registered is lost for the other with
	// no way to tell that apart from "the phase-2 receiver never got it"
	// (BUG-2742). Counting is what makes it two — waiting for "a subscriber"
	// is satisfied by whichever registers first.
	waitForSubscriberCount(t, mr, redisns.Default.Name(redisChannelSuffix)+"ws-1", 2)

	recv := func(t *testing.T, ch chan Event, who string) Event {
		t.Helper()
		select {
		case e := <-ch:
			return e
		case <-time.After(3 * time.Second):
			t.Fatalf("%s never received the event", who)
			return Event{}
		}
	}

	// Phase 1 publishes the bare form. BOTH must deliver it.
	phase1.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1", ItemID: "from-phase-1"})
	if got := recv(t, ch1, "phase-1 receiver"); got.ItemID != "from-phase-1" {
		t.Fatalf("phase-1 receiver got %+v", got)
	}
	bareOnTwo := recv(t, ch2, "phase-2 receiver")
	if bareOnTwo.ItemID != "from-phase-1" {
		t.Fatalf("phase-2 receiver got %+v", bareOnTwo)
	}

	// Phase 2 publishes the prefixed form. BOTH must deliver it — the phase-1
	// receiver is running a binary that ACCEPTS the prefix even though it does
	// not emit one, which is the whole reason phase 1 must be rolled first.
	phase2.Publish(Event{Type: ItemCreated, WorkspaceID: "ws-1", ItemID: "from-phase-2"})
	prefixedOnOne := recv(t, ch1, "phase-1 receiver")
	if prefixedOnOne.ItemID != "from-phase-2" {
		t.Fatalf("phase-1 receiver got %+v", prefixedOnOne)
	}
	if got := recv(t, ch2, "phase-2 receiver"); got.ItemID != "from-phase-2" {
		t.Fatalf("phase-2 receiver got %+v", got)
	}

	// PREMISE, so this is not passing on two buses that both happened to
	// publish the same shape: the two payload forms really did differ.
	if prefixedOnOne.ID <= bareOnTwo.ID {
		t.Fatalf("fixture: the second publish should carry a higher id, got %d after %d", prefixedOnOne.ID, bareOnTwo.ID)
	}
	phase2.mu.Lock()
	adopted := phase2.epoch
	phase2.mu.Unlock()
	if adopted == 0 {
		t.Fatal("fixture: the phase-2 receiver should have adopted an epoch from the prefixed message")
	}

	// And both can still answer a resume, which is what the buffers are for.
	if got := phase1.EventsSince("ws-1", prefixedOnOne.ID); got == nil {
		t.Fatal("the phase-1 receiver must be able to answer a resume after the mixed run")
	}
	if got := phase2.EventsSince("ws-1", prefixedOnOne.ID); got == nil {
		t.Fatal("the phase-2 receiver must be able to answer a resume after the mixed run")
	}
}

func TestARepeatedIDIsTreatedAsBackwards(t *testing.T) {
	// codex round 12: the guard is `<=`, and every test used a strictly lower
	// id — so a `<` implementation passed them all while letting a REPEATED id
	// into the buffer. That is a duplicate delivery and a replay that can
	// serve the same id twice, with no reset reported.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	_, gen := liveGen(t, b, "ws-1")

	b.fanOutFromRedis(gen, 3, Event{ID: 300, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 3, Event{ID: 300, Type: ItemUpdated, WorkspaceID: "ws-1"})

	_, resets := obs.snapshot()
	if len(resets) != 1 || resets[0] != ResetReasonCounterBackward {
		t.Fatalf("a repeated id must be reported as %s, got %v", ResetReasonCounterBackward, resets)
	}
}

func TestAColdJoinAcceptsTheAdjacentCursor(t *testing.T) {
	// CHARACTERIZATION of the residual this unit deliberately leaves open, so
	// it is executable rather than only described (codex round 12).
	//
	// A bus with EMPTY buffers adopts an epoch without dropping anything, so
	// its first buffer starts at exactly the first id it sees — and a client
	// holding the id one below that, from a space this process never saw, is
	// SERVED. Closing it locally means refusing the adjacent cursor forever,
	// which trades a rare silent skip for a common extra resync: a client
	// legitimately holds 149 from replica A and reconnects to replica B whose
	// first id is 150, routinely.
	//
	// If this test starts failing because someone made the adjacent cursor
	// refuse, that is not necessarily wrong — but it is the load trade above,
	// and it should be a decision rather than a side effect.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	_, gen := liveGen(t, b, "ws-1")

	b.fanOutFromRedis(gen, 9, Event{ID: 150, Type: ItemUpdated, WorkspaceID: "ws-1"})

	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("a cold join must not report a reset — that would give the metric a per-deploy baseline; got %v", resets)
	}
	if got := b.EventsSince("ws-1", 149); got == nil {
		t.Fatal("the adjacent cursor is ACCEPTED today; see this test's comment before changing that")
	}
	// The boundary, so the characterization is exact rather than approximate:
	// one lower than adjacent leaves room for a missed event and IS refused.
	if got := b.EventsSince("ws-1", 148); got != nil {
		t.Fatalf("a cursor with room for a missed event must be a gap, got %d events", len(got))
	}
}

func TestAPayloadThatDecodesButIsNotOurEventEndsCoverage(t *testing.T) {
	// codex round 15. Two payloads decode without error and are not a usable
	// event: one that is valid JSON and empty, and one naming a different
	// workspace from the channel it arrived on. Both used to be skipped
	// silently — the first fell out of fan-out because a zero Event has no
	// workspace to match a subscription, the second would have been appended
	// to the OTHER workspace's buffer.
	for _, tc := range []struct {
		name    string
		payload func(epoch int64) string
	}{
		{name: "valid JSON, empty event", payload: func(e int64) string {
			return strconv.FormatInt(e, 10) + "|4242|null"
		}},
		{name: "body names a different workspace", payload: func(e int64) string {
			body, _ := json.Marshal(Event{ID: 0, Type: ItemUpdated, WorkspaceID: "ws-elsewhere"})
			return strconv.FormatInt(e, 10) + "|4243|" + string(body)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, mr := newFlippedRedisBus(t)
			obs := &recordingObserver{}
			b.SetObserver(obs)

			ch, _ := b.Subscribe("ws-1")
			defer b.Unsubscribe(ch)
			// Registration before the first publish — see BUG-2742 and the
			// note on TestAPrefixedPayloadReachesReconciliation...
			waitForSubscribers(t, mr, redisns.Default.Name(redisChannelSuffix)+"ws-1", true)
			b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

			var first Event
			select {
			case first = <-ch:
			case <-time.After(3 * time.Second):
				t.Fatal("the first event never arrived")
			}
			b.mu.Lock()
			epoch := b.epoch
			b.mu.Unlock()
			// PREMISE: coverage exists, and we know the live generation, so
			// the payload below is refused for its SHAPE rather than for
			// carrying a stale epoch.
			if got := b.EventsSince("ws-1", first.ID); got == nil {
				t.Fatal("fixture: coverage must be established first")
			}
			if epoch <= 0 {
				t.Fatalf("fixture: expected an adopted generation, got %d", epoch)
			}

			if err := b.client.Publish(context.Background(),
				redisns.Default.Name(redisChannelSuffix)+"ws-1", tc.payload(epoch)).Err(); err != nil {
				t.Fatalf("publish: %v", err)
			}

			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) && b.EventsSince("ws-1", first.ID) != nil {
				time.Sleep(2 * time.Millisecond)
			}
			if got := b.EventsSince("ws-1", first.ID); got != nil {
				t.Fatalf("a payload we cannot use is a hole; the resume must be a gap, got %d events", len(got))
			}
			_, resets := obs.snapshot()
			if len(resets) == 0 || resets[len(resets)-1] != ResetReasonUndecodableMessage {
				t.Fatalf("want %s, got %v", ResetReasonUndecodableMessage, resets)
			}
		})
	}
}

func TestAStragglerFromBeforeAResetIsBoundedByTheNextNewSpaceEvent(t *testing.T) {
	// codex round 17 named a window the design leaves open: once an epoch is
	// adopted, a BARE message is treated as belonging to the current space,
	// and it does — unless the counter reset between that publisher's
	// assignment and its publish.
	//
	// The comment at that branch says the window closes when this WORKSPACE
	// next receives a lower id. That is a mechanical claim, so it is asserted
	// here rather than only written down — and the boundary is asserted with
	// it: the closure is per workspace, and the second case below shows it
	// does NOT fire when other workspaces have carried the shared counter past
	// the straggler's value first.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	_, gen := liveGen(t, b, "ws-1")

	// The new space, after a reset: generation 4, ids from 1.
	b.fanOutFromRedis(gen, 4, Event{ID: 1, Type: ItemUpdated, WorkspaceID: "ws-1"})

	// The straggler: assigned 100 in the DEAD space, published bare after the
	// reset, so it carries no epoch to give it away.
	b.fanOutFromRedis(gen, 0, Event{ID: 100, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("nothing can detect the straggler ON ARRIVAL; a reset here would mean the discriminator exists after all, got %v", resets)
	}

	// The next event from the NEW space is lower than the straggler, which is
	// what closes the window.
	b.fanOutFromRedis(gen, 4, Event{ID: 2, Type: ItemUpdated, WorkspaceID: "ws-1"})

	_, resets := obs.snapshot()
	if len(resets) != 1 || resets[0] != ResetReasonCounterBackward {
		t.Fatalf("the next new-space event must close the window loudly, got %v", resets)
	}
	// And the buffer that held the straggler is gone, so nothing is replayed
	// across the boundary after this point.
	if got := b.EventsSince("ws-1", 1); got != nil {
		t.Fatalf("a cursor inside the window must be refused once it closes, got %d events", len(got))
	}
}

func TestTheStragglerWindowDoesNotCloseWhenOtherWorkspacesCarryTheCounter(t *testing.T) {
	// THE BOUNDARY of the test above, and a residual rather than a defect to
	// fix here (codex round 21). The counter is global and the
	// counter-backwards check is per workspace, so if OTHER workspaces consume
	// ids past the straggler's value before this one publishes again, this
	// workspace's next id is HIGHER and nothing fires. The dead-space id stays
	// in the buffer.
	//
	// Closing it needs a global high-water mark, which fires on interleaves
	// across any pair of workspaces during the phase-2 roll — the storm that
	// round 9 armed the check against. Asserted so the residual is executable
	// and so a future global check announces itself here rather than in a
	// deployment.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	_, genA := liveGen(t, b, "ws-a")
	_, genB := liveGen(t, b, "ws-b")

	b.fanOutFromRedis(genA, 4, Event{ID: 1, Type: ItemUpdated, WorkspaceID: "ws-a"})
	// The straggler, from the dead space, on ws-a.
	b.fanOutFromRedis(genA, 0, Event{ID: 100, Type: ItemUpdated, WorkspaceID: "ws-a"})
	// Another workspace carries the shared counter past it.
	b.fanOutFromRedis(genB, 4, Event{ID: 150, Type: ItemUpdated, WorkspaceID: "ws-b"})
	// ws-a's next event is therefore HIGHER than the straggler.
	b.fanOutFromRedis(genA, 4, Event{ID: 151, Type: ItemUpdated, WorkspaceID: "ws-a"})

	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("today nothing detects this; a reset here means a global check was added — see this test's comment, got %v", resets)
	}
	if got := b.EventsSince("ws-a", 99); got == nil {
		t.Fatal("today the dead-space id is served; a gap here means a global check was added")
	}
}

func TestAStragglerDoesNotEraseTheCounterBackwardsFloor(t *testing.T) {
	// codex round 20, an INTERACTION between two fixes that were each correct
	// alone. A lower generation inside the straggler window drops the buffers
	// — and it used to do so with the same argument an epoch change uses,
	// which CLEARS the floor. But a straggler is not proof of a new space;
	// that is the question the window exists to defer.
	//
	// The sequence it produced: a counter-backwards reset raises the floor to
	// 100, a straggler clears it, bare traffic repopulates from 51, and a
	// client resuming from 99 is served later ids as though coverage were
	// continuous — silently skipping 51..99.
	b := newTestRedisBus(t)
	_, gen := liveGen(t, b, "ws-1")

	b.fanOutFromRedis(gen, 5, Event{ID: 100, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, 5, Event{ID: 60, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if b.discardedHighWater != 100 {
		t.Fatalf("fixture: the counter-backwards reset should have raised the floor to 100, got %d", b.discardedHighWater)
	}

	// The straggler, inside the window.
	b.fanOutFromRedis(gen, 4, Event{ID: 900, Type: ItemUpdated, WorkspaceID: "ws-1"})

	if b.discardedHighWater != 100 {
		t.Fatalf("a straggler must not clear a floor it cannot know is obsolete; it became %d", b.discardedHighWater)
	}

	// The consequence, asserted rather than inferred: traffic below the mark
	// repopulates the buffer, and a cursor below the mark is still refused.
	b.fanOutFromRedis(gen, 5, Event{ID: 51, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if got := b.EventsSince("ws-1", 99); got != nil {
		t.Fatalf("a cursor below the discarded high-water mark must still be a gap, got %d events", len(got))
	}
	// Control: a genuine epoch change DOES clear it, or the floor becomes
	// permanent and every cursor is refused until a dead counter is passed.
	b.fanOutFromRedis(gen, 6, Event{ID: 1, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if b.discardedHighWater != 0 {
		t.Fatalf("an epoch change must clear the floor, got %d", b.discardedHighWater)
	}
}

func TestAWrongTypedEpochKeyIsRecoveredToo(t *testing.T) {
	// codex round 20. The corrupted-epoch recovery ran on the VALUE, after a
	// bare GET — and a GET on a key holding a list raises WRONGTYPE, aborting
	// the script before the recovery could run. The guard written to handle a
	// corrupted epoch was unreachable for one of the ways an epoch gets
	// corrupted, and every phase-2 publish failed until someone deleted the
	// key by hand.
	b, mr := newFlippedRedisBus(t)
	epochKey := redisns.Default.Name(redisEpochSuffix)
	next := listen(t, b.client, redisns.Default.Name(redisChannelSuffix)+"ws-1")

	// PUBLISH ONCE FIRST, and this is load-bearing rather than setup. The
	// script's id == 1 branch SETs the epoch unconditionally, and SET replaces
	// a key of any type — so on a fresh counter the wrong-typed key is
	// overwritten before the GET is ever reached, and the branch under test is
	// never entered. Found by the mutation matrix: without this publish the
	// test passed with the TYPE check deleted.
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	if _, _, err := decodePayload(next()); err != nil {
		t.Fatalf("fixture: the first publish must succeed, got %v", err)
	}

	// The key now holds a string, so it has to be removed before a list can
	// take its place — which is exactly the state an operator or a colliding
	// installation would leave behind.
	if err := b.client.Del(context.Background(), epochKey).Err(); err != nil {
		t.Fatalf("clear the epoch key: %v", err)
	}
	if _, err := b.client.RPush(context.Background(), epochKey, "not", "a", "string").Result(); err != nil {
		t.Fatalf("seed a list at the epoch key: %v", err)
	}
	if got := mr.Type(epochKey); got != "list" {
		t.Fatalf("fixture: the epoch key should hold a list, it holds %q", got)
	}

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "item-7"})

	epoch, ev, err := decodePayload(next())
	if err != nil {
		t.Fatalf("the publish must survive a wrong-typed epoch key: %v", err)
	}
	if epoch <= 0 {
		t.Fatalf("want a positive generation after recovery, got %d", epoch)
	}
	if ev.ItemID != "item-7" {
		t.Fatalf("the event must still carry its body, got %+v", ev)
	}
	if got := mr.Type(epochKey); got != "string" {
		t.Fatalf("the key must be recovered to a string, it holds %q", got)
	}
}
