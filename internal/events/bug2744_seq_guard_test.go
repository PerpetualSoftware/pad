package events

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/redisns"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// BUG-2744: the event sequence counter had no value guard, and the id was
// stringified through a Lua double.
//
// A corrupted counter (another type, a non-integer, or 18+ digits) made INCR
// abort the script, or let a value through that receivers could not trust, so
// every publish failed until someone repaired the key by hand. Lead ruling,
// day 89: SELF-HEAL, VISIBLY. The guard deletes the key, the INCR returns 1,
// and the existing id == 1 rotation starts a new id space, which every
// receiver answers with sync_required. The repair is logged and counted,
// because a heal nobody sees would hide whatever corrupted the key.

func newSeqGuardBus(t *testing.T, publishEpoch bool) (*RedisBus, *recordingObserver, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBusWithKeys(client, redisns.Default, publishEpoch, false)
	obs := &recordingObserver{}
	b.SetObserver(obs)
	t.Cleanup(b.Close)
	return b, obs, mr
}

func receiveN(t *testing.T, ch chan Event, n int) []Event {
	t.Helper()
	var got []Event
	for i := 0; i < n; i++ {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("subscriber channel closed")
			}
			got = append(got, ev)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for delivery %d of %d", i+1, n)
		}
	}
	return got
}

type seqCorruption struct {
	name  string
	shape string
	seed  func(t *testing.T, c *redis.Client, key string)
}

func seqCorruptions() []seqCorruption {
	set := func(v string) func(t *testing.T, c *redis.Client, key string) {
		return func(t *testing.T, c *redis.Client, key string) {
			if err := c.Set(context.Background(), key, v, 0).Err(); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	return []seqCorruption{
		{"list", SeqRepairWrongType, func(t *testing.T, c *redis.Client, key string) {
			if err := c.RPush(context.Background(), key, "not", "a", "counter").Err(); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}},
		{"hash", SeqRepairWrongType, func(t *testing.T, c *redis.Client, key string) {
			if err := c.HSet(context.Background(), key, "f", "v").Err(); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}},
		{"non-numeric string", SeqRepairNotInteger, set("abc")},
		{"negative integer", SeqRepairNotInteger, set("-5")},
		{"18 digits", SeqRepairTooLarge, set("100000000000000000")},
		{"int64 max", SeqRepairTooLarge, set("9223372036854775807")},
	}
}

// Phase 2, every shape: the publish SUCCEEDS, starts a new space at id 1, is
// reported once with its shape, and a cursor from the old space is refused
// (sync_required) instead of being served the new space's ids.
func TestACorruptedSequenceCounterHealsVisiblyIntoANewSpace(t *testing.T) {
	seqKey := redisns.Default.Name(redisSeqSuffix)
	epochKey := redisns.Default.Name(redisEpochSuffix)
	ctx := context.Background()

	for _, tc := range seqCorruptions() {
		t.Run(tc.name, func(t *testing.T) {
			b, obs, _ := newSeqGuardBus(t, true)
			ch, _, _ := b.Subscribe(ctx, "ws-1")
			defer b.Unsubscribe(ch)

			for _, id := range []string{"a1", "a2", "a3"} {
				if err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: id}); err != nil {
					t.Fatalf("publish %s: %v", id, err)
				}
			}
			old := receiveN(t, ch, 3)
			cursor := old[2].ID
			// CONTROLS: a healthy counter reports no repair, and the buffer
			// serves a cursor inside the space before the corruption.
			if got := obs.seqRepairList(); len(got) != 0 {
				t.Fatalf("control: a healthy counter reported repairs %v", got)
			}
			if b.EventsSince("ws-1", old[0].ID) == nil {
				t.Fatal("control: a cursor inside the first space must be served")
			}
			epochBefore, err := b.client.Get(ctx, epochKey).Result()
			if err != nil {
				t.Fatalf("read the epoch: %v", err)
			}

			if err := b.client.Del(ctx, seqKey).Err(); err != nil {
				t.Fatalf("clear the counter: %v", err)
			}
			tc.seed(t, b.client, seqKey)

			if err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "b1"}); err != nil {
				t.Fatalf("the publish must survive a %s counter: %v", tc.name, err)
			}
			ev := receiveN(t, ch, 1)[0]
			if ev.ItemID != "b1" || ev.ID != 1 {
				t.Fatalf("want b1 at id 1, the first id of a new space; got %+v", ev)
			}

			if got := obs.seqRepairList(); len(got) != 1 || got[0] != tc.shape {
				t.Fatalf("want exactly one repair reported as %q, got %v", tc.shape, got)
			}
			if got, err := b.client.Get(ctx, seqKey).Result(); err != nil || got != "1" {
				t.Fatalf("the counter must restart at 1, got %q (err %v)", got, err)
			}
			epochAfter, err := b.client.Get(ctx, epochKey).Result()
			if err != nil {
				t.Fatalf("read the epoch: %v", err)
			}
			if epochAfter == epochBefore {
				t.Fatalf("the new space must carry a new epoch; both are %s", epochBefore)
			}

			// THE CONSEQUENCE: the receiver drops what it held, and the old
			// cursor is refused rather than served the new space's ids.
			obs.awaitReset(t, ResetReasonEpochChange, 3*time.Second)
			if got := b.EventsSince("ws-1", cursor); got != nil {
				t.Fatalf("a cursor from the abandoned space must be refused (sync_required), got %d events", len(got))
			}
		})
	}
}

// Phase 1 (the assign script, then a plain PUBLISH) runs the same guard: the
// publish survives and the repair is reported.
//
// What it does NOT get is sync_required, and that is a pre-existing,
// documented limit rather than part of this fix: a receiver on a NEVER-flipped
// deployment (this fixture) has adopted no epoch, and the backwards-id arm
// runs only once one is adopted (see fanOut: phase-1 publishers interleave as ordinary traffic,
// so firing there would resync every client in the default configuration).
// A counter DELETED by hand on phase 1 goes undetected the same way today, and
// the heal deletes it, so the two are now the same case; phase 2 is what
// closes it. Pinned below so a change to that limit is seen here, not assumed.
func TestACorruptedSequenceCounterHealsOnThePhaseOnePath(t *testing.T) {
	seqKey := redisns.Default.Name(redisSeqSuffix)
	ctx := context.Background()

	for _, tc := range seqCorruptions() {
		t.Run(tc.name, func(t *testing.T) {
			b, obs, _ := newSeqGuardBus(t, false)
			ch, _, _ := b.Subscribe(ctx, "ws-1")
			defer b.Unsubscribe(ch)

			for _, id := range []string{"a1", "a2", "a3"} {
				if err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: id}); err != nil {
					t.Fatalf("publish %s: %v", id, err)
				}
			}
			receiveN(t, ch, 3)

			if err := b.client.Del(ctx, seqKey).Err(); err != nil {
				t.Fatalf("clear the counter: %v", err)
			}
			tc.seed(t, b.client, seqKey)

			if err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "b1"}); err != nil {
				t.Fatalf("the publish must survive a %s counter: %v", tc.name, err)
			}
			if ev := receiveN(t, ch, 1)[0]; ev.ID != 1 {
				t.Fatalf("want id 1, the first of a new space; got %+v", ev)
			}
			if got := obs.seqRepairList(); len(got) != 1 || got[0] != tc.shape {
				t.Fatalf("want exactly one repair reported as %q, got %v", tc.shape, got)
			}
			// THE LIMIT, pinned on what defines it: no reset. An old cursor can
			// still be refused right now, but only incidentally: the buffer's
			// newest id (1) is below it. Once the new space climbs past the
			// cursor it would be served from a buffer holding both spaces,
			// which is the pre-existing phase-1 hole, not something to assert.
			if _, resets := obs.snapshot(); len(resets) != 0 {
				t.Fatalf("the documented phase-1 limit changed: a counter restart now reports %v. "+
					"Update this test and the limit's notes in fanOut", resets)
			}
		})
	}
}

// The precision half: a 17-digit counter is usable (no repair), and the id on
// the wire is the one the key holds. Through a Lua double, 99999999999999999
// renders as 1e+17.
func TestTheIDOnTheWireIsTheStoredOneAboveExactDoubleRange(t *testing.T) {
	seqKey := redisns.Default.Name(redisSeqSuffix)
	ctx := context.Background()
	const seeded = "99999999999999998"
	want, _ := strconv.ParseInt("99999999999999999", 10, 64)

	for _, phase2 := range []bool{true, false} {
		t.Run("phase2="+strconv.FormatBool(phase2), func(t *testing.T) {
			b, obs, _ := newSeqGuardBus(t, phase2)
			ch, _, _ := b.Subscribe(ctx, "ws-1")
			defer b.Unsubscribe(ch)

			// One ordinary publish first, so the seeded publish is not the
			// counter's first and does not take the id == 1 rotation.
			if err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "a1"}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			receiveN(t, ch, 1)
			if err := b.client.Set(ctx, seqKey, seeded, 0).Err(); err != nil {
				t.Fatalf("seed: %v", err)
			}

			if err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "b1"}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if ev := receiveN(t, ch, 1)[0]; ev.ID != want {
				t.Fatalf("the id on the wire is %d, want the stored %d", ev.ID, want)
			}
			if got := obs.seqRepairList(); len(got) != 0 {
				t.Fatalf("a 17-digit counter is usable and must not be repaired, got %v", got)
			}
		})
	}
}
