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
			"body is not JSON":    "7|77|not json",
			"neither form":        "not json at all",
			"prefix without id":   "7|" + string(body),
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
	ch := b.Subscribe(workspaceID)
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
	// The mixed-VERSION ordering case: a phase-1 publisher assigns and
	// publishes in two calls, so a lower id can land after a higher one. The
	// events between the two are unrecoverable here, so every cursor at or
	// below what we discarded is refused.
	b := newTestRedisBus(t)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	_, gen := liveGen(t, b, "ws-1")

	b.fanOutFromRedis(gen, 0, Event{ID: 200, Type: ItemUpdated, WorkspaceID: "ws-1"})
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
	if _, resets := obs.snapshot(); len(resets) != 1 {
		t.Fatalf("a straggler must not cause a second reset, got %v", resets)
	}
	// And it must not be BUFFERED, or the dead space's id sits in a live
	// buffer and a resume can be answered across the boundary.
	if rb, ok := b.replayBuffers["ws-a"]; ok && rb.count != 0 {
		t.Fatalf("the straggler must not be appended; ws-a's buffer holds %d events", rb.count)
	}

	// Control: the live generation still buffers normally, so the discard is
	// scoped to the abandoned space rather than to the workspace.
	b.fanOutFromRedis(genA, 2, Event{ID: 2, Type: ItemUpdated, WorkspaceID: "ws-a"})
	if got := b.EventsSince("ws-a", 2); got == nil {
		t.Fatal("a message in the live generation must establish coverage")
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
	b, _ := newFlippedRedisBus(t)

	ch := b.Subscribe("ws-1")
	defer b.Unsubscribe(ch)

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
