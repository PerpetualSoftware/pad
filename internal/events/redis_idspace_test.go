package events

import (
	"context"
	"encoding/json"
	"strings"
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
		epoch, ev, err := decodePayload("e-1|77|" + string(body))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if epoch != "e-1" {
			t.Fatalf("epoch: want e-1, got %q", epoch)
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
		if epoch != "" {
			t.Fatalf("a bare payload carries no id-space information; got epoch %q", epoch)
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
		if epoch != "" || ev.ID != 9 || ev.Title != "a|b|c" {
			t.Fatalf("want bare decode with title intact, got epoch=%q id=%d title=%q", epoch, ev.ID, ev.Title)
		}
	})

	t.Run("rejections", func(t *testing.T) {
		body, _ := json.Marshal(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
		for name, payload := range map[string]string{
			"empty epoch":       "|77|" + string(body),
			"non-integer id":    "e-1|seventy|" + string(body),
			"body is not JSON":  "e-1|77|not json",
			"neither form":      "not json at all",
			"prefix without id": "e-1|" + string(body),
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

		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})

		epoch, ev, err := decodePayload(next())
		if err != nil {
			t.Fatalf("a flipped instance must emit a decodable payload: %v", err)
		}
		if epoch == "" {
			t.Fatal("a flipped instance must emit an epoch")
		}
		if ev.ID != 1 {
			t.Fatalf("first id from a fresh counter must be 1, got %d", ev.ID)
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
	}

	first, err := publishScript.Run(b.ctx, b.client, keys, string(body), "epoch-candidate", redisDedupeTTLSeconds).Int64()
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first != 1 {
		t.Fatalf("first run must assign id 1, got %d", first)
	}

	second, err := publishScript.Run(b.ctx, b.client, keys, string(body), "epoch-candidate", redisDedupeTTLSeconds).Int64()
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
	b.fanOutFromRedis(gen, "epoch-a", Event{ID: 10, Type: ItemUpdated, WorkspaceID: "ws-1"})

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
	b.fanOutFromRedis(gen, "", Event{ID: 5, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if got := b.EventsSince("ws-1", 5); got == nil {
		t.Fatal("fixture: the bare event must establish coverage first")
	}

	b.fanOutFromRedis(gen, "epoch-a", Event{ID: 6, Type: ItemUpdated, WorkspaceID: "ws-1"})

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

	b.fanOutFromRedis(gen1, "epoch-a", Event{ID: 100, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen2, "epoch-a", Event{ID: 101, Type: ItemUpdated, WorkspaceID: "ws-2"})

	// A backwards id inside epoch-a raises the floor to 101.
	b.fanOutFromRedis(gen1, "epoch-a", Event{ID: 50, Type: ItemUpdated, WorkspaceID: "ws-1"})
	if b.discardedHighWater != 101 {
		t.Fatalf("fixture: floor should stand at 101, got %d", b.discardedHighWater)
	}

	// Now the space itself changes.
	b.fanOutFromRedis(gen1, "epoch-b", Event{ID: 1, Type: ItemUpdated, WorkspaceID: "ws-1"})

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

	b.fanOutFromRedis(gen, "", Event{ID: 200, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, "", Event{ID: 150, Type: ItemUpdated, WorkspaceID: "ws-1"})

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
	b.fanOutFromRedis(gen, "", Event{ID: 201, Type: ItemUpdated, WorkspaceID: "ws-1"})
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

	b.fanOutFromRedis(gen, "epoch-a", Event{ID: 10, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, "", Event{ID: 11, Type: ItemUpdated, WorkspaceID: "ws-1"})
	b.fanOutFromRedis(gen, "epoch-a", Event{ID: 12, Type: ItemUpdated, WorkspaceID: "ws-1"})

	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("a mixed-format stream inside one epoch must report no resets, got %v", resets)
	}
	if got := b.EventsSince("ws-1", 10); got == nil || len(got) != 2 {
		t.Fatalf("coverage must span the mixed-format run, got %v", got)
	}
	if b.epoch != "epoch-a" {
		t.Fatalf("the adopted epoch must survive a bare message, got %q", b.epoch)
	}
}
