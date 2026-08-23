package events

import (
	"context"
	"strconv"
	"testing"

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
