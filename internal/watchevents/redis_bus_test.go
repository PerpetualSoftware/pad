package watchevents

// BUG-2651 — RedisBus.
//
// WHAT IS AND IS NOT COVERED HERE, stated up front because the gap is real:
// there is no Redis in CI and no miniredis/redismock in go.mod (internal/events'
// RedisBus has no tests at all for the same reason). So these tests drive the
// two halves that do not need a server —
//
//   - the LOCAL half (fanOutLocally + Subscribe/SubscribeAndReplaySince/
//     Unsubscribe/Close), which is where the concurrency contract lives and
//     where a port of events.RedisBus's two-lock layout would break;
//   - Publish's fail-closed branch, driven with a client pointed at a closed
//     port, which is a real INCR failure rather than a simulated one;
//   - the JSON round trip a notification takes through Redis.
//
// NOT covered: the actual pub/sub round trip (channel name, subscription
// lifecycle against a live server). That needs a Redis, and adding one is a
// dependency decision rather than a test-writing one.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// deadClient returns a Redis client pointed at a closed port on loopback.
// Every command against it fails immediately — a genuine connection error, not
// a stub — which is exactly the condition Publish's fail-closed branch is for.
func deadClient(t *testing.T) *redis.Client {
	t.Helper()
	return redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  -1,
	})
}

// newLocalOnlyBus builds a RedisBus without starting its subscription, for the
// tests that exercise the local fan-out path directly. Constructing through
// NewRedisBus would spawn a receive goroutine dialing a dead server, which adds
// noise and nothing else — the receive loop's only job is to call
// fanOutLocally, which these tests call themselves.
//
// ctx/cancel are populated even though nothing here uses the context, because
// Close legitimately assumes the constructor set them. Making Close nil-safe
// instead would have been the wrong fix: a zero-value RedisBus is not a
// supported construction, and a defensive nil check there would mask real
// misuse to spare a test fixture.
func newLocalOnlyBus(size int) *RedisBus {
	ctx, cancel := context.WithCancel(context.Background())
	return &RedisBus{
		subscribers: make(map[chan Notification]struct{}),
		replay:      newReplayBuffer(size),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// TestRedisBusSubscribeAndReplayIsAtomic is the load-bearing test.
//
// The contract (inherited from MemoryBus, and the reason Bus has this method
// at all) is that a notification is EITHER in the replay slice OR delivered on
// the channel — never both, never neither. events.RedisBus cannot provide it:
// it guards subscribers and replay with different locks. So a future
// "simplification" that aligns this type with that template must fail here.
//
// Asserting the exclusion rather than the presence is the point: a build that
// delivered every notification twice would satisfy "the resume worked".
func TestRedisBusSubscribeAndReplayIsAtomic(t *testing.T) {
	b := newLocalOnlyBus(64)
	defer b.Close()

	// Three notifications land before anyone subscribes.
	for i := int64(1); i <= 3; i++ {
		b.fanOutLocally(Notification{ID: i, Kind: KindComment, ItemRef: "TASK-1"})
	}

	ch, missed := b.SubscribeAndReplaySince(1)

	if len(missed) != 2 {
		t.Fatalf("replay since 1 should carry ids 2,3; got %d entries: %+v", len(missed), missed)
	}
	for _, n := range missed {
		if n.ID <= 1 {
			t.Errorf("replay carried id %d, which is not above sinceID=1", n.ID)
		}
	}

	// Nothing already-replayed may ALSO be sitting on the channel.
	select {
	case n := <-ch:
		t.Fatalf("notification id=%d was replayed AND delivered on the channel — "+
			"the subscribe and the buffer read did not share a critical section", n.ID)
	default:
	}

	// A notification fanned out after the subscribe goes to the channel and
	// must NOT be something the caller already had.
	b.fanOutLocally(Notification{ID: 4, Kind: KindComment, ItemRef: "TASK-1"})
	select {
	case n := <-ch:
		if n.ID != 4 {
			t.Errorf("expected id 4 on the channel, got %d", n.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("notification fanned out after subscribe never arrived on the channel")
	}
}

// TestRedisBusSubscribeAndReplayHasNoWindowUnderConcurrency is the same
// contract driven by a race rather than by a fixed ordering: a fan-out loop
// runs while a subscriber joins, and every id must appear exactly once across
// (replay ∪ channel).
//
// Run this with -race; without the shared lock it fails on the duplicate long
// before the race detector has anything to say.
func TestRedisBusSubscribeAndReplayHasNoWindowUnderConcurrency(t *testing.T) {
	// Buffer > total so nothing is evicted and every id is accounted for.
	const total = 1000
	b := newLocalOnlyBus(4096)
	defer b.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); i <= total; i++ {
			b.fanOutLocally(Notification{ID: i, Kind: KindStatusChange, ItemRef: "TASK-1"})
			// Deliberately paced. The first version of this test fired all
			// of them as fast as a map iteration allows and then slept 1ms
			// before subscribing — so the producer was always FINISHED by
			// the time the subscriber joined, the channel leg received
			// nothing, and the test passed on the replay slice alone. It
			// was vacuous for the defect it names, and a split-lock mutant
			// survived 50 runs of it before this was noticed.
			time.Sleep(10 * time.Microsecond)
		}
	}()

	// Join mid-flight.
	time.Sleep(2 * time.Millisecond)
	ch, missed := b.SubscribeAndReplaySince(0)

	wg.Wait()

	seen := make(map[int64]int, total)
	for _, n := range missed {
		seen[n.ID]++
	}
	// Drain on a QUIET deadline rather than `default`: sends happen on the
	// producer goroutine, so a non-blocking drain can declare the channel
	// empty while a send is still in flight — another way to be vacuous.
	deadline := time.NewTimer(200 * time.Millisecond)
	defer deadline.Stop()
	fromChannel := 0
drain:
	for {
		select {
		case n, ok := <-ch:
			if !ok {
				break drain
			}
			seen[n.ID]++
			fromChannel++
			if !deadline.Stop() {
				<-deadline.C
			}
			deadline.Reset(50 * time.Millisecond)
		case <-deadline.C:
			break drain
		}
	}

	for id, count := range seen {
		if count != 1 {
			t.Fatalf("notification id=%d appeared %d times across replay+channel; "+
				"exactly-once is the contract SubscribeAndReplaySince exists to provide", id, count)
		}
	}

	// THE PRECONDITION THAT MAKES THE ASSERTION ABOVE MEAN ANYTHING. If the
	// subscriber joined before the first fan-out or after the last, there was
	// no window to test and "no duplicates" is trivially true. Both legs must
	// be non-empty for this run to have exercised the boundary at all.
	if len(missed) == 0 || fromChannel == 0 {
		t.Fatalf("fixture did not straddle the subscribe: %d replayed, %d on the channel — "+
			"the run proves nothing about the window", len(missed), fromChannel)
	}
}

// TestRedisBusSubscribeAndReplayNeverDoubleDelivers is the DETECTOR for the
// split-lock defect, and it exists because the test above turned out not to be
// one.
//
// Measured: with subscribe and replay in two separate critical sections (the
// events.RedisBus layout), the test above killed the mutant in roughly 1 run
// in 20 — 0 of 5 independent invocations. A single subscribe is one chance at
// a window a few nanoseconds wide, so a test built on one subscribe is a
// coin-flip dressed as an assertion.
//
// This takes many chances instead: a producer runs continuously while
// subscribers join over and over, and each join checks the ONE signature the
// defect leaves — an id that appears in the replay slice AND then arrives on
// the channel. No accounting of the whole id space, no timing assumptions
// beyond "drain briefly"; just the intersection, which must be empty every
// time.
func TestRedisBusSubscribeAndReplayNeverDoubleDelivers(t *testing.T) {
	const attempts = 600

	b := newLocalOnlyBus(4096)
	defer b.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			b.fanOutLocally(Notification{ID: i, Kind: KindStatusChange, ItemRef: "TASK-1"})
			time.Sleep(5 * time.Microsecond)
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	straddled := 0
	for i := 0; i < attempts; i++ {
		ch, missed := b.SubscribeAndReplaySince(0)

		replayed := make(map[int64]struct{}, len(missed))
		for _, n := range missed {
			replayed[n.ID] = struct{}{}
		}

		// NON-BLOCKING drain, and the reason is what makes this test both
		// fast and exact. In the split-lock layout the duplicate is SENT
		// before the buffer is read — register, unlock, [fan-out sends],
		// lock, read — so by the time this call has returned, the duplicate
		// is already sitting in the channel's buffer. Waiting on a timer
		// bought nothing but 11 seconds of CI time.
		got := 0
	drain:
		for {
			select {
			case n, ok := <-ch:
				if !ok {
					break drain
				}
				got++
				if _, dup := replayed[n.ID]; dup {
					b.Unsubscribe(ch)
					t.Fatalf("attempt %d: notification id=%d was in the replay slice AND arrived on the "+
						"channel — subscribe and the buffer read did not share one critical section", i, n.ID)
				}
			default:
				break drain
			}
		}
		if len(missed) > 0 && got > 0 {
			straddled++
		}
		b.Unsubscribe(ch)
	}

	// Same anti-vacuity guard as above, applied to the whole run: if no
	// attempt ever saw both a replay and a live delivery, this test spun
	// without ever approaching the boundary and its silence means nothing.
	if straddled == 0 {
		t.Fatalf("none of %d attempts saw both replayed and live notifications; "+
			"the producer and the subscriber never overlapped", attempts)
	}
	t.Logf("%d/%d attempts straddled the subscribe boundary", straddled, attempts)
}

// commandRecorder is a go-redis Hook that records every command NAME the
// client attempts, whether or not it succeeds. It is the seam that makes the
// fail-closed decision observable without a live server: the policy difference
// is not "was anything delivered" (with Redis down, nothing is delivered under
// either policy — the first version of this test asserted exactly that and a
// local-counter mutant sailed through it), it is WHICH COMMANDS Publish
// issues.
type commandRecorder struct {
	mu   sync.Mutex
	cmds []string
}

func (r *commandRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }
func (r *commandRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (r *commandRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.mu.Lock()
		r.cmds = append(r.cmds, cmd.Name())
		r.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (r *commandRecorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

// TestRedisBusPublishFailsClosedWhenIDsAreUnavailable pins the one place this
// implementation deliberately diverges from events.RedisBus, which falls back
// to a local counter when INCR fails.
//
// WHAT IT ASSERTS AND WHY IT IS SHAPED THIS WAY. A local-counter fallback
// would mint an id from a counter another instance is also using, and
// replayBuffer.since() reasons on monotonicity, so the damage is silent replay
// corruption rather than a visible error. But "nothing was delivered" cannot
// detect that: Publish never delivers locally (the receive path does), so with
// Redis down NOTHING is delivered under either policy. Asserting delivery was
// the first version of this test, and a fallback mutant survived it.
//
// So the assertion is on the COMMANDS ISSUED — fail-closed attempts INCR and
// then stops; a fallback would go on to attempt PUBLISH with the locally
// minted id. That is the observable difference between the two policies, and
// it is observable without a server.
func TestRedisBusPublishFailsClosedWhenIDsAreUnavailable(t *testing.T) {
	rec := &commandRecorder{}
	client := deadClient(t)
	client.AddHook(rec)

	b := newLocalOnlyBus(16)
	b.client = client
	defer b.Close()

	ch := b.Subscribe()
	b.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1", Summary: "should not be published"})

	cmds := rec.names()
	if len(cmds) == 0 {
		t.Fatal("no Redis command was attempted at all; the fixture is not exercising Publish")
	}
	// The id assignment and the publish must be ONE round trip (publishScript),
	// not a bare INCR followed by a bare PUBLISH. Two calls are what let two
	// instances interleave and publish their ids out of order — see
	// publishScript's comment. Seeing either bare command here means the
	// atomicity fix was undone.
	for _, name := range cmds {
		if name == "incr" || name == "publish" {
			t.Fatalf("Publish issued a bare %q (commands: %v) — id assignment and publication must be "+
				"one atomic script, or two instances can publish their ids out of order", name, cmds)
		}
	}

	// Belt and braces: nothing may have leaked into local state either.
	select {
	case n, ok := <-ch:
		if ok {
			t.Fatalf("a notification was delivered (id=%d) despite INCR failing", n.ID)
		}
	default:
	}
	if got := b.EventsSince(0); len(got) != 0 {
		t.Fatalf("replay buffer holds %d entries after a failed publish: %+v", len(got), got)
	}
}

// TestRedisBusNotificationSurvivesTheJSONRoundTrip covers the half of the
// Redis hop that does not need Redis: every field a consumer gates on has to
// survive marshal/unmarshal, because the receive path rebuilds the struct from
// bytes rather than sharing memory with the publisher.
//
// The fields chosen are the ones watchNotificationVisible actually branches on
// — a silently-dropped TargetSessionID would route a push to every session of
// a user instead of one, which no test of the bus's plumbing would notice.
func TestRedisBusNotificationSurvivesTheJSONRoundTrip(t *testing.T) {
	want := Notification{
		ID:              42,
		WorkspaceID:     "ws-1",
		ItemID:          "item-1",
		CollectionID:    "coll-1",
		ItemRef:         "TASK-214",
		Kind:            KindPush,
		Actor:           "agent",
		ActorName:       "Wren",
		Summary:         "look at this",
		AssignedUserID:  "user-1",
		TargetUserID:    "user-2",
		TargetSessionID: "sess-9",
		StatusFieldKey:  "status",
		ToStatus:        "done",
		Timestamp:       1755000000000,
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Notification
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("notification did not survive the round trip:\n got: %+v\nwant: %+v", got, want)
	}
}

// TestRedisBusReplayReportsAHoleAsAGap — codex round 3.
//
// MemoryBus assigns every id itself, so its replay buffer is contiguous by
// construction and the ONLY gap it can report is eviction. RedisBus receives
// ids over at-most-once pub/sub, so it can miss one: buffer holds 100 and 102,
// is nowhere near full, and the underlying replayBuffer.since() would answer a
// resume from 100 with just [102] — losing 101 silently, with no sync_required
// for the consumer to act on.
//
// The assertion is on the RESUME THAT SPANS THE HOLE returning nil, and — the
// part that keeps it from being over-broad — on the resumes that do NOT span
// it still working. A bus that answered nil to everything after one hole would
// satisfy the first half alone.
func TestRedisBusReplayReportsAHoleAsAGap(t *testing.T) {
	b := newLocalOnlyBus(64)
	defer b.Close()

	b.fanOutLocally(Notification{ID: 99, Kind: KindComment, ItemRef: "TASK-1"})
	b.fanOutLocally(Notification{ID: 100, Kind: KindComment, ItemRef: "TASK-1"})
	// 101 is never received — the subscription blipped.
	b.fanOutLocally(Notification{ID: 102, Kind: KindComment, ItemRef: "TASK-1"})

	if got := b.EventsSince(100); got != nil {
		t.Errorf("a resume from 100 must span the missing 101 and report a gap; got %+v", got)
	}
	if got := b.EventsSince(99); got != nil {
		t.Errorf("a resume from 99 also spans the hole; got %+v", got)
	}

	// Resumes that do not span the hole still work.
	after := b.EventsSince(101)
	if after == nil {
		t.Fatal("a resume from 101 does not span the hole and must replay normally, not report a gap")
	}
	if len(after) != 1 || after[0].ID != 102 {
		t.Errorf("resume from 101: got %+v, want just id 102", after)
	}
	if latest := b.EventsSince(102); len(latest) != 0 {
		t.Errorf("resume from the newest id should be caught-up-empty, got %+v", latest)
	}

	// And a fresh subscriber (sinceID 0) is not resuming from a position, so
	// it must not be handed a gap signal for a hole it never spanned.
	if fresh := b.EventsSince(0); fresh == nil {
		t.Error("sinceID=0 is a fresh subscriber, not a resume; it must not report a gap")
	}
}

// TestRedisBusDecodePayloadRoundTrip covers the wire format publishScript
// introduced (Codex round 1 P1): the id is assigned inside the Lua script and
// prepended as "<id>|<json>", so the publisher never knows it and the receiver
// is the only place the two halves are rejoined.
//
// The malformed cases are here because this decoder consumes bytes from a
// shared channel that any process with the Redis credentials can publish to; a
// panic or a silently-zero id would both be worse than a logged error.
func TestRedisBusDecodePayloadRoundTrip(t *testing.T) {
	want := Notification{
		WorkspaceID:     "ws-1",
		ItemRef:         "TASK-214",
		Kind:            KindPush,
		TargetUserID:    "user-2",
		TargetSessionID: "sess-9",
		Timestamp:       1755000000000,
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := decodePayload("77|" + string(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != 77 {
		t.Errorf("id: got %d, want 77 — the id lives in the prefix, not the JSON", got.ID)
	}
	want.ID = 77
	if got != want {
		t.Errorf("payload did not survive the round trip:\n got: %+v\nwant: %+v", got, want)
	}

	// A '|' inside the body must not confuse the split: only the FIRST one
	// separates, which is what makes the format unambiguous.
	withPipe := Notification{ItemRef: "TASK-1", Summary: "a|b|c"}
	pipeBody, err := json.Marshal(withPipe)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	gotPipe, err := decodePayload("5|" + string(pipeBody))
	if err != nil {
		t.Fatalf("decode with a pipe in the body: %v", err)
	}
	if gotPipe.Summary != "a|b|c" || gotPipe.ID != 5 {
		t.Errorf("pipe in the body broke the split: id=%d summary=%q", gotPipe.ID, gotPipe.Summary)
	}

	for _, bad := range []struct {
		name    string
		payload string
	}{
		{"no separator", `{"ItemRef":"TASK-1"}`},
		{"non-numeric id", `abc|{"ItemRef":"TASK-1"}`},
		{"body is not JSON", `1|not json`},
		{"empty", ``},
	} {
		t.Run("malformed/"+bad.name, func(t *testing.T) {
			if _, err := decodePayload(bad.payload); err == nil {
				t.Fatalf("decodePayload(%q) returned no error; a malformed payload from the shared "+
					"channel must be rejected, not fanned out with a zero id", bad.payload)
			}
		})
	}
}

// TestRedisBusCloseIsIdempotentAndClosesSubscribers — Close runs from server
// shutdown while a receive goroutine may be mid-fan-out, so it has to be safe
// to call twice and must not leave a subscriber holding a channel nobody will
// close.
func TestRedisBusCloseIsIdempotentAndClosesSubscribers(t *testing.T) {
	b := newLocalOnlyBus(16)
	ch := b.Subscribe()

	b.Close()
	b.Close() // must not panic on a double close of the same channels

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("subscriber channel yielded a value after Close; expected it closed")
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber channel was not closed by Close — the consumer would block forever")
	}

	// A subscriber arriving after Close must get a closed channel rather than
	// registering into a bus that will never close it.
	late := b.Subscribe()
	select {
	case _, ok := <-late:
		if ok {
			t.Fatal("post-Close Subscribe yielded a value; expected an already-closed channel")
		}
	case <-time.After(time.Second):
		t.Fatal("post-Close Subscribe returned a live channel nobody will ever close")
	}
}
