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

// BUG-2740 — the generation counter had no guard, while the epoch key beside
// it did.
//
// Every branch that rotates the id space INCRs pad:event_epoch_gen, and an
// INCR against a corrupted key aborts the script AFTER the sequence INCR has
// already landed. Redis does not roll back a script's earlier writes, so the
// failure burns an id (a hole to every receiver), repeats on the next publish,
// and never self-heals — the branch that would rotate the generation is the
// branch that cannot run.
//
// The filing named the two WRONGTYPE cases. A probe against the pinned
// miniredis found four, and the other two are STRING values that pass any type
// check: a non-numeric one, and one that overflows int64 on increment. Each
// gets its own case here for that reason — a guard that checks only TYPE
// passes half this table.

// seedFn pins the restart seed so the repaired value can be asserted exactly
// rather than bounded, which is what lets a mutation that repairs to the WRONG
// value be told apart from one that repairs correctly.
const fixedSeed int64 = 1700000000

func newSeededFlippedBus(t *testing.T) (*RedisBus, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBusWithKeys(client, redisns.Default, true)
	b.nowUnix = func() int64 { return fixedSeed }
	t.Cleanup(b.Close)
	return b, mr
}

func TestACorruptedGenerationCounterIsRepairedRatherThanFatal(t *testing.T) {
	genKey := redisns.Default.Name(redisEpochGenSuffix)
	seqKey := redisns.Default.Name(redisSeqSuffix)
	epochKey := redisns.Default.Name(redisEpochSuffix)

	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, c *redis.Client)
		abort string
	}{
		{"list", func(t *testing.T, c *redis.Client) {
			if err := c.RPush(context.Background(), genKey, "not", "a", "counter").Err(); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}, "WRONGTYPE"},
		{"hash", func(t *testing.T, c *redis.Client) {
			if err := c.HSet(context.Background(), genKey, "f", "v").Err(); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}, "WRONGTYPE"},
		{"non-numeric string", func(t *testing.T, c *redis.Client) {
			if err := c.Set(context.Background(), genKey, "abc", 0).Err(); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}, "value is not an integer"},
		{"string overflowing int64", func(t *testing.T, c *redis.Client) {
			if err := c.Set(context.Background(), genKey, "9223372036854775807", 0).Err(); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}, "increment would overflow"},
		{"zero", func(t *testing.T, c *redis.Client) {
			if err := c.Set(context.Background(), genKey, "0", 0).Err(); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}, "not a positive generation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, mr := newSeededFlippedBus(t)
			next := listen(t, b.client, redisns.Default.Name(redisChannelSuffix)+"ws-1")
			ctx := context.Background()

			// PUBLISH ONCE FIRST, load-bearing rather than setup — the same
			// trap the epoch key's test records. The id == 1 branch rotates
			// unconditionally, so on a fresh counter the corrupted key would
			// be repaired on a path this table is not testing. One publish
			// puts the sequence past 1 so the later publish reaches the
			// branch under test.
			b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
			if _, _, err := decodePayload(next()); err != nil {
				t.Fatalf("fixture: the first publish must succeed, got %v", err)
			}

			// Clear the epoch so the SECOND publish takes a rotating branch,
			// and corrupt the generation counter under it.
			if err := b.client.Del(ctx, epochKey).Err(); err != nil {
				t.Fatalf("clear the epoch: %v", err)
			}
			if err := b.client.Del(ctx, genKey).Err(); err != nil {
				t.Fatalf("clear the generation key: %v", err)
			}
			tc.seed(t, b.client)

			seqBefore, err := b.client.Get(ctx, seqKey).Int64()
			if err != nil {
				t.Fatalf("read the sequence: %v", err)
			}

			b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "item-7"})

			epoch, ev, err := decodePayload(next())
			if err != nil {
				t.Fatalf("the publish must survive a %s generation key (%s): %v", tc.name, tc.abort, err)
			}
			if ev.ItemID != "item-7" {
				t.Fatalf("the event must still carry its body, got %+v", ev)
			}

			// THE REPAIRED VALUE, ASSERTED EXACTLY. Bounding it (">0", or
			// "large") would pass against a repair to 1 — which is the
			// specific wrong answer the ruling exists to rule out, because a
			// generation BELOW ones receivers have already adopted reads as a
			// regression rather than a rotation.
			if epoch != fixedSeed {
				t.Fatalf("want the generation restarted at the wall-clock seed %d, got %d", fixedSeed, epoch)
			}
			if got := mr.Type(genKey); got != "string" {
				t.Fatalf("the generation key must be repaired to a string, it holds %q", got)
			}
			if got, err := b.client.Get(ctx, genKey).Result(); err != nil || got != strconv.FormatInt(fixedSeed, 10) {
				t.Fatalf("the generation key must hold the seed, got %q (err %v)", got, err)
			}

			// AND THE SEQUENCE ADVANCED EXACTLY ONCE. This is the leg that
			// pins the actual damage: an aborted script leaves the sequence
			// INCRemented with nothing published, which every receiver reads
			// as a hole. A repair that merely stopped the error while losing
			// the publish would satisfy every assertion above.
			seqAfter, err := b.client.Get(ctx, seqKey).Int64()
			if err != nil {
				t.Fatalf("read the sequence: %v", err)
			}
			if seqAfter != seqBefore+1 {
				t.Fatalf("the sequence must advance exactly once per published event: %d -> %d", seqBefore, seqAfter)
			}
			if ev.ID != seqAfter {
				t.Fatalf("the published id %d must be the sequence value %d", ev.ID, seqAfter)
			}
		})
	}
}

// The control: a HEALTHY generation counter is incremented, not replaced by
// the seed. Without this leg the table above passes against a guard that
// repairs unconditionally — which would restart the generation on every
// rotation and make the counter meaningless.
func TestAHealthyGenerationCounterIsIncrementedNotReseeded(t *testing.T) {
	b, _ := newSeededFlippedBus(t)
	epochKey := redisns.Default.Name(redisEpochSuffix)
	next := listen(t, b.client, redisns.Default.Name(redisChannelSuffix)+"ws-1")
	ctx := context.Background()

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	first, _, err := decodePayload(next())
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if first != 1 {
		t.Fatalf("a fresh installation's first generation must be 1, got %d", first)
	}

	// Force another rotation with the counter intact.
	if err := b.client.Del(ctx, epochKey).Err(); err != nil {
		t.Fatalf("clear the epoch: %v", err)
	}
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "item-2"})
	second, _, err := decodePayload(next())
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}

	if second != 2 {
		t.Fatalf("a healthy counter must INCREMENT: want generation 2, got %d", second)
	}
	if second == fixedSeed {
		t.Fatalf("a healthy counter was reseeded to the wall-clock value %d", fixedSeed)
	}
}

// ALL THREE ROTATION BRANCHES, because the table above reaches only one of
// them and a mutation said so: guarding just the id == 1 site survived the
// whole suite.
//
// publishScript rotates the id space from three places — a sequence starting
// at 1, an absent epoch on a sequence already in flight, and a corrupted
// epoch — and each INCRs the generation counter. A guard on one is a guard on
// none, since the other two abort the script exactly as before. This drives
// each branch with the counter corrupted underneath it.
//
// The branch is selected by the STATE, not by an argument, so each case sets
// up the state that forces it and then asserts the same repair.
func TestEveryRotationBranchGuardsTheGenerationCounter(t *testing.T) {
	genKey := redisns.Default.Name(redisEpochGenSuffix)
	seqKey := redisns.Default.Name(redisSeqSuffix)
	epochKey := redisns.Default.Name(redisEpochSuffix)

	for _, tc := range []struct {
		branch  string
		arrange func(t *testing.T, b *RedisBus)
	}{
		{"sequence starting at 1", func(t *testing.T, b *RedisBus) {
			ctx := context.Background()
			// A fresh id space takes the id == 1 branch, which rotates
			// unconditionally.
			//
			// THE EPOCH IS LEFT IN PLACE AND VALID, which is what ISOLATES
			// this branch (codex round 3). Clearing it too — the obvious
			// setup — lets the absent-epoch branch fire instead and produce
			// an identical result, so the case passed with the id == 1 body
			// deleted entirely. A stale epoch surviving a restarted sequence
			// is also the real-world shape: the seq key is what gets evicted,
			// and the epoch is what is left pointing at the abandoned space.
			if err := b.client.Del(ctx, seqKey).Err(); err != nil {
				t.Fatalf("clear the sequence: %v", err)
			}
			if got, err := b.client.Get(ctx, epochKey).Result(); err != nil || got == "" {
				t.Fatalf("fixture: this case needs a live epoch to isolate the branch, got %q (err %v)", got, err)
			}
		}},
		{"absent epoch on a live sequence", func(t *testing.T, b *RedisBus) {
			ctx := context.Background()
			if err := b.client.Del(ctx, epochKey).Err(); err != nil {
				t.Fatalf("clear the epoch: %v", err)
			}
		}},
		{"corrupted epoch", func(t *testing.T, b *RedisBus) {
			ctx := context.Background()
			// A wrong-TYPED epoch reaches the recovery branch at the bottom
			// of the script — the one that rotates because the epoch itself
			// is unusable. Both shared keys corrupted at once is the shape a
			// namespace collision or a mixed restore actually produces.
			if err := b.client.Del(ctx, epochKey).Err(); err != nil {
				t.Fatalf("clear the epoch: %v", err)
			}
			if err := b.client.RPush(ctx, epochKey, "not", "an", "epoch").Err(); err != nil {
				t.Fatalf("corrupt the epoch: %v", err)
			}
		}},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			b, mr := newSeededFlippedBus(t)
			next := listen(t, b.client, redisns.Default.Name(redisChannelSuffix)+"ws-1")
			ctx := context.Background()

			// Get the sequence past 1 so the branches that need a live
			// sequence can be reached; the first case then clears it again.
			b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
			if _, _, err := decodePayload(next()); err != nil {
				t.Fatalf("fixture: the first publish must succeed, got %v", err)
			}

			tc.arrange(t, b)

			if err := b.client.Del(ctx, genKey).Err(); err != nil {
				t.Fatalf("clear the generation key: %v", err)
			}
			if err := b.client.RPush(ctx, genKey, "not", "a", "counter").Err(); err != nil {
				t.Fatalf("corrupt the generation key: %v", err)
			}

			b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "item-9"})

			epoch, ev, err := decodePayload(next())
			if err != nil {
				t.Fatalf("the %s branch must survive a corrupted generation counter: %v", tc.branch, err)
			}
			if ev.ItemID != "item-9" {
				t.Fatalf("the event must still carry its body, got %+v", ev)
			}
			if epoch != fixedSeed {
				t.Fatalf("the %s branch must restart the generation at the seed %d, got %d", tc.branch, fixedSeed, epoch)
			}
			if got := mr.Type(genKey); got != "string" {
				t.Fatalf("the generation key must be repaired to a string, it holds %q", got)
			}
		})
	}
}

// The generation reaching the wire must be the one in the key, at every
// magnitude the guard admits (BUG-2740, codex round 2).
//
// Redis hands an integer reply to Lua as a NUMBER, and Lua 5.1 numbers are
// doubles printed with %.14g — so tostring() stops being faithful BELOW the
// 18 digits this guard accepts. Measured at the boundary rather than reasoned
// about: a counter at 999999999999999998 increments to 999999999999999999,
// and tostring() renders that 1000000000000000000 while GET returns it
// exactly. The published epoch and the stored generation would disagree, and
// the receiver would adopt a generation the publisher does not hold.
//
// Divergence starts at 2^53, not at the guard's digit ceiling — doubles are
// exact only to 9007199254740992 — so this is live for a large part of the
// range the guard admits, not just at its edge. And such a magnitude is not
// one a generation counter reaches by counting: it is what a HAND-EDITED or
// collided key arrives at, which is the same class of event this whole guard
// exists for.
func TestThePublishedGenerationMatchesTheStoredOneAboveExactDoubleRange(t *testing.T) {
	b, _ := newSeededFlippedBus(t)
	genKey := redisns.Default.Name(redisEpochGenSuffix)
	epochKey := redisns.Default.Name(redisEpochSuffix)
	next := listen(t, b.client, redisns.Default.Name(redisChannelSuffix)+"ws-1")
	ctx := context.Background()

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1"})
	if _, _, err := decodePayload(next()); err != nil {
		t.Fatalf("fixture: the first publish must succeed, got %v", err)
	}

	// A valid generation INSIDE the guard, so it is INCREMENTED rather than
	// repaired — which is the path where the stringification happens.
	//
	// The magnitude is chosen, not arbitrary: doubles represent integers
	// exactly only to 2^53 (9007199254740992, 16 digits), so divergence
	// begins well below the 17-digit ceiling. Measured — this value
	// increments to 99999999999999999 in the key while tostring() renders it
	// 100000000000000000.
	const nearLimit = "99999999999999998"
	if err := b.client.Set(ctx, genKey, nearLimit, 0).Err(); err != nil {
		t.Fatalf("seed the generation: %v", err)
	}
	if err := b.client.Del(ctx, epochKey).Err(); err != nil {
		t.Fatalf("clear the epoch: %v", err)
	}

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "item-11"})
	published, _, err := decodePayload(next())
	if err != nil {
		t.Fatalf("publish at the guard's limit: %v", err)
	}

	stored, err := b.client.Get(ctx, genKey).Result()
	if err != nil {
		t.Fatalf("read the generation back: %v", err)
	}
	if strconv.FormatInt(published, 10) != stored {
		t.Fatalf("the published generation %d must equal the stored one %s — "+
			"tostring() of the INCR result loses precision at this magnitude; read the value back with GET",
			published, stored)
	}
	if stored != "99999999999999999" {
		t.Fatalf("the counter must have incremented exactly once, it holds %s", stored)
	}
}

// A generation at or above the guard's ceiling is repaired ONCE, not rotated
// twice inside one publish (BUG-2740, codex round 4).
//
// The two ceilings interact, and that is the whole point of this case. The
// value next_gen accepts is about to be INCREMENTED and the result becomes the
// EPOCH — and the epoch guard further down rejects anything over 18 digits. So
// a generation ceiling of 18 lets a counter at 999999999999999999 increment
// into a 19-digit epoch, the epoch guard fires, and the script rotates a
// SECOND time within one publish: it finds a 19-digit generation, repairs it
// to the wall-clock seed, and publishes a generation far BELOW the one
// receivers hold.
//
// The ceiling for what is USABLE therefore has to sit one digit under the
// ceiling for what is PUBLISHABLE. This pins both sides of that boundary.
func TestTheGenerationCeilingIsOneUnderTheEpochCeiling(t *testing.T) {
	genKey := redisns.Default.Name(redisEpochGenSuffix)
	epochKey := redisns.Default.Name(redisEpochSuffix)

	for _, tc := range []struct {
		name     string
		seed     string
		want     string
		repaired bool
	}{
		{"at the ceiling, incremented", "99999999999999997", "99999999999999998", false},
		{"over the ceiling, repaired once", "999999999999999999", strconv.FormatInt(fixedSeed, 10), true},
		// THE CASE THAT DISCRIMINATES THE CEILING ITSELF, and the reason the
		// two above do not: an 18-digit value whose increment STAYS 18
		// digits. With the ceiling at 17 it is over the line and repaired to
		// the seed; with the ceiling wrongly at 18 it is accepted and simply
		// incremented, and no second rotation occurs to bring it back to the
		// seed — so the two settings produce different published values here,
		// where for 999999999999999999 they produce the same one by different
		// routes. Found by mutation: without this row, moving the ceiling
		// back to 18 passes the whole suite.
		{"just over the ceiling, increment would be clean", "100000000000000000", strconv.FormatInt(fixedSeed, 10), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newSeededFlippedBus(t)
			next := listen(t, b.client, redisns.Default.Name(redisChannelSuffix)+"ws-1")
			ctx := context.Background()

			b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "a"})
			if _, _, err := decodePayload(next()); err != nil {
				t.Fatalf("fixture: %v", err)
			}

			if err := b.client.Set(ctx, genKey, tc.seed, 0).Err(); err != nil {
				t.Fatalf("seed the generation: %v", err)
			}
			if err := b.client.Del(ctx, epochKey).Err(); err != nil {
				t.Fatalf("clear the epoch: %v", err)
			}

			b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "b"})
			published, _, err := decodePayload(next())
			if err != nil {
				t.Fatalf("publish: %v", err)
			}

			stored, err := b.client.Get(ctx, genKey).Result()
			if err != nil {
				t.Fatalf("read the generation: %v", err)
			}
			if stored != tc.want {
				t.Fatalf("generation: want %s, got %s", tc.want, stored)
			}
			if strconv.FormatInt(published, 10) != stored {
				t.Fatalf("the published epoch %d must equal the stored generation %s", published, stored)
			}

			// THE LEG THAT CATCHES THE DOUBLE ROTATION. When the value is
			// repaired, it must land on the seed EXACTLY — a second rotation
			// would leave seed+1 behind, which is what an 18-digit ceiling
			// produced.
			if tc.repaired && stored != strconv.FormatInt(fixedSeed, 10) {
				t.Fatalf("a repair must rotate exactly once: want the seed %d, got %s", fixedSeed, stored)
			}
			if !tc.repaired && published == fixedSeed {
				t.Fatalf("a usable counter must be incremented, not reseeded to %d", fixedSeed)
			}
		})
	}
}

// A REPAIRED GENERATION CAN COLLIDE, AND WHAT CATCHES IT IS THE SEQUENCE, NOT
// THE EPOCH (BUG-2740, codex round 5 + the lead's probe request).
//
// The repair seeds from wall-clock seconds, which is above any COUNTED
// history but is not a monotonicity guarantee: corrupt the key twice inside
// one second and both repairs seed the same value, so two genuinely different
// id spaces carry the identical epoch. The epoch comparison cannot separate
// them — equal means "same space" by design, so epoch_regressed does not fire
// either.
//
// It is still not silent, and this test exists because that was folklore
// until it was measured. A second id space means ids are REISSUED, and a
// reissued id arrives at or below the receiver's high-water mark with no
// generation change — which is the counter_backward arm. Buffers dropped,
// floor raised, old cursors refused.
//
// Pinning the chain matters more than pinning the arm: the guarantee is
// carried by a DIFFERENT detector than the one the epoch mechanism suggests,
// so a future change that weakens counter_backward would remove a protection
// nothing here says it provides.
func TestACollidingRepairIsCaughtBySequenceRatherThanEpoch(t *testing.T) {
	b, _ := newSeededFlippedBus(t) // nowUnix pinned: both repairs seed the same value
	obs := &recordingObserver{}
	b.SetObserver(obs)

	genKey := redisns.Default.Name(redisEpochGenSuffix)
	seqKey := redisns.Default.Name(redisSeqSuffix)
	epochKey := redisns.Default.Name(redisEpochSuffix)
	ctx := context.Background()

	ch, _, _ := b.Subscribe(context.Background(), "ws-1")
	defer b.Unsubscribe(ch)

	waitFor := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			select {
			case _, ok := <-ch:
				if !ok {
					t.Fatal("subscriber channel closed")
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("timed out waiting for delivery %d of %d", i+1, n)
			}
		}
	}
	corrupt := func() {
		t.Helper()
		if err := b.client.Del(ctx, genKey).Err(); err != nil {
			t.Fatalf("clear the generation key: %v", err)
		}
		if err := b.client.RPush(ctx, genKey, "x").Err(); err != nil {
			t.Fatalf("corrupt the generation key: %v", err)
		}
	}

	// SPACE A, published under a repaired generation.
	corrupt()
	for _, id := range []string{"a1", "a2", "a3"} {
		b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: id})
	}
	waitFor(3)

	epochA, err := b.client.Get(ctx, epochKey).Result()
	if err != nil {
		t.Fatalf("read the epoch: %v", err)
	}
	if _, resets := obs.snapshot(); len(resets) != 0 {
		t.Fatalf("control: a repaired-but-consistent space must report no reset, got %v", resets)
	}
	// Control: the buffer vouches for this span right now.
	if b.EventsSince("ws-1", 1) == nil {
		t.Fatal("control: a cursor inside space A must be served before the collision")
	}

	// SPACE B: the sequence restarts AND the generation is corrupted again
	// inside the same second, so the repair lands on the same seed.
	if err := b.client.Del(ctx, seqKey).Err(); err != nil {
		t.Fatalf("reset the sequence: %v", err)
	}
	corrupt()
	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "b1"})
	waitFor(1)

	epochB, err := b.client.Get(ctx, epochKey).Result()
	if err != nil {
		t.Fatalf("read the epoch: %v", err)
	}

	// THE PREMISE OF THE TEST, asserted rather than assumed: the two spaces
	// really do collide on the epoch. If a future change makes the seed
	// monotonic this assertion fails, which is the correct signal — the test
	// would then be describing a state that can no longer occur.
	if epochA != epochB {
		t.Fatalf("this test needs the two spaces to share an epoch; got %s then %s", epochA, epochB)
	}

	_, resets := obs.snapshot()
	var sawBackward bool
	for _, r := range resets {
		if r == ResetReasonCounterBackward {
			sawBackward = true
		}
		if r == ResetReasonEpochRegressed || r == ResetReasonEpochChange {
			t.Fatalf("the epoch is unchanged, so no epoch-based reason should fire; got %v", resets)
		}
	}
	if !sawBackward {
		t.Fatalf("a colliding repair must be caught by the SEQUENCE going backwards (%q), got %v",
			ResetReasonCounterBackward, resets)
	}

	// And the consequence, not just the report: the old cursor is refused
	// rather than replayed the new space's events.
	if got := b.EventsSince("ws-1", 1); got != nil {
		t.Fatalf("a cursor from the abandoned space must be refused, got %d events", len(got))
	}
}

// A BROKEN HOST CLOCK MUST NOT BE FATAL (BUG-2740, codex round 7).
//
// The seed is an input, and an unset or misconfigured clock can report zero or
// a negative second. Before the clamp, the repair SET that at the generation
// key and returned it as the epoch — and because the epoch guard rejects
// anything not matching ^[1-9][0-9]*$, it rotated, called back into the
// repair, got the SAME bad seed, and assigned it to the epoch WITHOUT
// revalidating. An unparseable epoch reached the wire, which every receiver
// rejects: a total, permanent drop, arrived at through the mechanism meant to
// prevent it.
//
// SPLIT INTO A PURE TABLE PLUS ONE WIRED CASE, deliberately. The clamp's
// behaviour is arithmetic and needs no Redis; standing up a server per row
// bought nothing and cost five of them, on a package whose timing-sensitive
// tests are already load-fragile (BUG-2742). The end-to-end leg below is what
// stops the table being a unit test for a function nothing calls.
func TestClampGenerationSeed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		unix int64
		want string
	}{
		{"zero", 0, "1"},
		{"negative", -86400, "1"},
		{"one", 1, "1"},
		{"ordinary", 1700000000, "1700000000"},
		{"absurdly far future", 1 << 62, "99999999999999999"},
	} {
		if got := clampGenerationSeed(tc.unix); got != tc.want {
			t.Errorf("clampGenerationSeed(%d) = %s, want %s (%s)", tc.unix, got, tc.want, tc.name)
		}
	}
}

// THE WIRING, which the table above cannot prove: a clock that would have been
// fatal still publishes a decodable event with a usable epoch.
//
// Zero is the case to drive end to end — it is the one that reproduced the
// original failure, since "0" fails the epoch guard's ^[1-9] and sent the
// script back into the repair for the same bad seed.
func TestABrokenClockDoesNotProduceAnUnpublishableEpoch(t *testing.T) {
	genKey := redisns.Default.Name(redisEpochGenSuffix)
	epochKey := redisns.Default.Name(redisEpochSuffix)

	b, _ := newSeededFlippedBus(t)
	b.nowUnix = func() int64 { return 0 }

	next := listen(t, b.client, redisns.Default.Name(redisChannelSuffix)+"ws-1")
	ctx := context.Background()

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "a"})
	if _, _, err := decodePayload(next()); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// Clear the epoch, or no rotation branch fires and the repair never runs
	// — the second publish would just reuse the epoch the fixture minted. An
	// earlier version of this test missed that and passed vacuously wherever
	// the expected clamp happened to equal that epoch.
	if err := b.client.Del(ctx, epochKey).Err(); err != nil {
		t.Fatalf("clear the epoch: %v", err)
	}
	if err := b.client.Del(ctx, genKey).Err(); err != nil {
		t.Fatalf("clear the generation key: %v", err)
	}
	if err := b.client.RPush(ctx, genKey, "x").Err(); err != nil {
		t.Fatalf("corrupt the generation key: %v", err)
	}

	b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "b"})
	epoch, ev, err := decodePayload(next())
	if err != nil {
		t.Fatalf("a repair under a zero clock must still publish a decodable event: %v", err)
	}
	if ev.ItemID != "b" {
		t.Fatalf("the event must carry its body, got %+v", ev)
	}
	if epoch != 1 {
		t.Fatalf("want the clamped seed 1 on the wire, got %d", epoch)
	}
}
