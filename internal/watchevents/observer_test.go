package watchevents

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// recordingObserver is the instrument under which the tests below run. It
// counts, rather than merely noting, so a test can distinguish "reported
// once" from "reported on every subscriber in the loop" — a real
// difference, since the drop site lives inside a per-subscriber loop.
type recordingObserver struct {
	mu sync.Mutex

	dropped       map[string]int
	gaps          int
	resumeGaps    int
	missed        int64
	resets        map[string]int
	loopExits     int
	probeFailures int
	totalEvents   int
}

func newRecordingObserver() *recordingObserver {
	return &recordingObserver{
		dropped: map[string]int{},
		resets:  map[string]int{},
	}
}

func (o *recordingObserver) NotificationDropped(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dropped[reason]++
	o.totalEvents++
}

func (o *recordingObserver) SequenceGap(missing int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gaps++
	o.missed += missing
	o.totalEvents++
}

func (o *recordingObserver) ResumeGap() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resumeGaps++
	o.totalEvents++
}

func (o *recordingObserver) SequenceReset(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resets[reason]++
	o.totalEvents++
}

func (o *recordingObserver) ReceiveLoopExited() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.loopExits++
	o.totalEvents++
}

func (o *recordingObserver) HeartbeatPublishFailed() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.probeFailures++
	o.totalEvents++
}

// observerCounts is the lock-free snapshot type. Separate from
// recordingObserver so returning one does not copy a sync.Mutex — which
// `go vet`'s copylocks check rejects, and which `go test`'s reduced vet
// subset does NOT run, so the suite was green while `make lint` would
// have failed.
type observerCounts struct {
	dropped       map[string]int
	resets        map[string]int
	gaps          int
	resumeGaps    int
	missed        int64
	loopExits     int
	probeFailures int
	totalEvents   int
}

func (o *recordingObserver) snapshot() observerCounts {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := observerCounts{
		dropped:       map[string]int{},
		resets:        map[string]int{},
		gaps:          o.gaps,
		resumeGaps:    o.resumeGaps,
		missed:        o.missed,
		loopExits:     o.loopExits,
		probeFailures: o.probeFailures,
		totalEvents:   o.totalEvents,
	}
	for k, v := range o.dropped {
		cp.dropped[k] = v
	}
	for k, v := range o.resets {
		cp.resets[k] = v
	}
	return cp
}

// TestMemoryBusReportsSlowSubscriberDrop asserts the DROP increments the
// observer — not merely that an observer can be attached.
//
// It asserts its own premise first: a subscriber with room receives the
// notification and produces NO drop report. Without that leg the test
// would pass against a bus that reported a drop on every publish, which
// is a differently-broken instrument that looks identical from the
// assertion that only checks the full case.
func TestMemoryBusReportsSlowSubscriberDrop(t *testing.T) {
	t.Parallel()

	b := New()
	defer b.Close()
	obs := newRecordingObserver()
	b.SetObserver(obs)

	ch, _ := b.Subscribe()

	// PREMISE: a healthy subscriber is delivered to and reports nothing.
	if err := b.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("premise failed: a subscriber with room did not receive the notification")
	}
	if got := obs.snapshot(); got.totalEvents != 0 {
		t.Fatalf("premise failed: healthy delivery reported %d observer events, want 0 (%+v)", got.totalEvents, got)
	}

	// Fill the subscriber's 64-deep buffer without draining it.
	for i := 0; i < 64; i++ {
		if err := b.Publish(Notification{Kind: KindPush, ItemRef: "TASK-2"}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if got := obs.snapshot(); got.totalEvents != 0 {
		t.Fatalf("premise failed: filling the buffer to capacity reported %d events, want 0 (%+v)", got.totalEvents, got)
	}

	// The 65th has nowhere to go.
	if err := b.Publish(Notification{Kind: KindPush, ItemRef: "TASK-3"}); err != nil {
		t.Fatalf("overflow publish: %v", err)
	}

	got := obs.snapshot()
	if got.dropped[DropReasonSlowSubscriber] != 1 {
		t.Fatalf("dropped[%s] = %d, want 1 (%+v)", DropReasonSlowSubscriber, got.dropped[DropReasonSlowSubscriber], got)
	}
	if got.totalEvents != 1 {
		t.Fatalf("total observer events = %d, want exactly 1 — the drop and nothing else (%+v)", got.totalEvents, got)
	}
}

// TestRedisBusReportsSlowSubscriberDrop is the REDIS bus's own drop
// site. The two implementations have separate fan-out loops in separate
// files, so one being instrumented says nothing about the other.
func TestRedisBusReportsSlowSubscriberDrop(t *testing.T) {
	t.Parallel()

	b, _ := newMiniredisBus(t, 128)
	obs := newRecordingObserver()
	b.SetObserver(obs)

	ch, _ := b.Subscribe()

	// PREMISE: a subscriber with room is delivered to and reports
	// nothing, so the assertion below is about the OVERFLOW rather than
	// about a bus that reports on every notification.
	b.fanOutLocally(Notification{ID: 1, Kind: KindPush, ItemRef: "TASK-1"}, b.currentGen())
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("premise failed: a subscriber with room did not receive the notification")
	}
	if got := obs.snapshot(); got.totalEvents != 0 {
		t.Fatalf("premise failed: healthy delivery reported %d observer events (%+v)", got.totalEvents, got)
	}

	// Fill the 64-deep subscriber buffer without draining, then overflow.
	// Ids stay contiguous so no gap is reported and the drop is the only
	// event in the snapshot.
	for i := int64(2); i <= 65; i++ {
		b.fanOutLocally(Notification{ID: i, Kind: KindPush, ItemRef: "TASK-2"}, b.currentGen())
	}
	if got := obs.snapshot(); got.totalEvents != 0 {
		t.Fatalf("premise failed: filling to capacity reported %d events (%+v)", got.totalEvents, got)
	}

	b.fanOutLocally(Notification{ID: 66, Kind: KindPush, ItemRef: "TASK-3"}, b.currentGen())

	got := obs.snapshot()
	if got.dropped[DropReasonSlowSubscriber] != 1 {
		t.Fatalf("dropped[%s] = %d, want 1 (%+v)", DropReasonSlowSubscriber, got.dropped[DropReasonSlowSubscriber], got)
	}
	if got.totalEvents != 1 {
		t.Fatalf("total observer events = %d, want exactly 1 — the drop and nothing else (%+v)", got.totalEvents, got)
	}
}

// TestRedisBusReportsSequenceGap drives fanOutLocally directly, which is
// where the detection lives, and pins BOTH numbers: that a gap was
// reported once, and how many notifications it spanned. One counter
// cannot distinguish one gap of 500 from 500 gaps of one, which is why
// the observer reports both and why this asserts both.
func TestRedisBusReportsSequenceGap(t *testing.T) {
	t.Parallel()

	b, _ := newMiniredisBus(t, 64)
	obs := newRecordingObserver()
	b.SetObserver(obs)

	// PREMISE: contiguous ids report nothing. Without this leg a bus that
	// reported a gap on every notification would pass the assertion below.
	b.fanOutLocally(Notification{ID: 1, Kind: KindPush}, b.currentGen())
	b.fanOutLocally(Notification{ID: 2, Kind: KindPush}, b.currentGen())
	if got := obs.snapshot(); got.totalEvents != 0 {
		t.Fatalf("premise failed: contiguous ids 1,2 reported %d observer events, want 0 (%+v)", got.totalEvents, got)
	}

	// 3 and 4 are missing.
	b.fanOutLocally(Notification{ID: 5, Kind: KindPush}, b.currentGen())

	got := obs.snapshot()
	if got.gaps != 1 {
		t.Fatalf("gaps = %d, want 1 (%+v)", got.gaps, got)
	}
	if got.missed != 2 {
		t.Fatalf("missed = %d, want 2 — ids 3 and 4 (%+v)", got.missed, got)
	}
}

// TestRedisBusReportsSequenceResets covers the two ID-SPACE reset reasons,
// which are detected in different functions (fanOutFromRedis for the epoch,
// fanOutLocally for the counter) and would be easy to wire one of and
// call done. The other two — subscription_resumed and undecodable_message,
// added by BUG-2739 — are detected on the receive loop and covered in
// redis_reconnect_test.go, which needs a severable connection to reach them.
func TestRedisBusReportsSequenceResets(t *testing.T) {
	t.Parallel()

	t.Run("epoch change", func(t *testing.T) {
		b, _ := newMiniredisBus(t, 64)
		obs := newRecordingObserver()
		b.SetObserver(obs)

		b.fanOutFromRedis("epoch-a", Notification{ID: 1, Kind: KindPush}, b.currentGen())
		if got := obs.snapshot(); got.totalEvents != 0 {
			t.Fatalf("premise failed: the first epoch seen reported %d events, want 0 (%+v)", got.totalEvents, got)
		}

		b.fanOutFromRedis("epoch-b", Notification{ID: 1, Kind: KindPush}, b.currentGen())

		got := obs.snapshot()
		if got.resets[ResetReasonEpochChange] != 1 {
			t.Fatalf("resets[%s] = %d, want 1 (%+v)", ResetReasonEpochChange, got.resets[ResetReasonEpochChange], got)
		}
	})

	t.Run("counter backwards", func(t *testing.T) {
		b, _ := newMiniredisBus(t, 64)
		obs := newRecordingObserver()
		b.SetObserver(obs)

		b.fanOutLocally(Notification{ID: 100, Kind: KindPush}, b.currentGen())
		if got := obs.snapshot(); got.totalEvents != 0 {
			t.Fatalf("premise failed: a cold start reported %d events, want 0 (%+v)", got.totalEvents, got)
		}

		// The shared counter restarted: an id at or below the high-water
		// mark.
		b.fanOutLocally(Notification{ID: 1, Kind: KindPush}, b.currentGen())

		got := obs.snapshot()
		if got.resets[ResetReasonCounterBackward] != 1 {
			t.Fatalf("resets[%s] = %d, want 1 (%+v)", ResetReasonCounterBackward, got.resets[ResetReasonCounterBackward], got)
		}
		if got.gaps != 0 {
			t.Fatalf("gaps = %d, want 0 — a backwards counter is a reset, not a gap (%+v)", got.gaps, got)
		}
	})
}

// TestRedisBusReportsReceiveLoopExit closes the client underneath a
// running bus, which is the ONE condition go-redis actually closes a
// subscription's message channel for (pool.ErrClosed — every other
// receive error is retried indefinitely and a health-check goroutine
// reconnects). Driving the real condition rather than calling the
// reporter directly is the difference between testing the wiring and
// testing the constant.
func TestRedisBusReportsReceiveLoopExit(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	b := NewRedisBusWithReplaySize(client, 64)
	defer b.Close()
	obs := newRecordingObserver()
	b.SetObserver(obs)

	// PREMISE: the bus is genuinely receiving before we break it —
	// otherwise a bus that never started would pass by reporting an exit
	// for the wrong reason.
	ch, _ := b.Subscribe()
	if err := b.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1"}); err != nil {
		t.Fatalf("premise publish: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("premise failed: the bus never delivered a round-tripped notification")
	}
	if got := obs.snapshot(); got.loopExits != 0 {
		t.Fatalf("premise failed: %d loop exits before closing the client (%+v)", got.loopExits, got)
	}

	// Close the CLIENT, not the bus: b.ctx stays live, so the receive
	// loop takes the closed-channel branch rather than the ctx branch.
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if obs.snapshot().loopExits == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("receive loop exit was never reported (%+v)", obs.snapshot())
}

// TestRedisBusCloseDoesNotReportAReceiveLoopExit: a NORMAL shutdown must
// be silent, or pad_watchevents_receive_loop_exits_total ticks on every
// deploy and the meaning it carries ("non-zero outside shutdown") is
// worthless.
//
// WHAT IT DISCRIMINATES, measured: removing the receive loop's ctx case
// makes it fail. It does NOT discriminate the ctx re-check inside the
// closed-channel branch — see that guard's own comment, which says so.
//
// The iteration loop would surface a nondeterministic version of the
// failure rather than a one-in-two flake.
func TestRedisBusCloseDoesNotReportAReceiveLoopExit(t *testing.T) {
	t.Parallel()

	const iterations = 40
	for i := 0; i < iterations; i++ {
		mr := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

		b := NewRedisBusWithReplaySize(client, 8)
		obs := newRecordingObserver()
		b.SetObserver(obs)

		// PREMISE: the loop is genuinely running, so a clean shutdown is
		// what is being measured rather than a bus that never started.
		ch, _ := b.Subscribe()
		if err := b.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1"}); err != nil {
			t.Fatalf("iteration %d premise publish: %v", i, err)
		}
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("iteration %d premise failed: no round-trip delivery", i)
		}

		b.Close()
		_ = client.Close()

		// Close waits for the receive goroutine, so by here the loop has
		// taken whichever branch it was going to take.
		if got := obs.snapshot(); got.loopExits != 0 {
			t.Fatalf("iteration %d: a normal Close reported %d receive-loop exits, want 0 (%+v)", i, got.loopExits, got)
		}
	}
}

// TestRedisBusReportsResumeGaps covers both routes to an unservable
// resume — the shared counter disagreeing, and the local coverage window
// — plus the control leg, because "always resync" would pass either
// positive case alone and is a different bug with the same counter
// reading.
func TestRedisBusReportsResumeGaps(t *testing.T) {
	t.Parallel()

	t.Run("a resume this instance cannot serve", func(t *testing.T) {
		t.Parallel()
		b, mr := newMiniredisBus(t, 64)
		obs := newRecordingObserver()
		b.SetObserver(obs)

		ch, _ := b.Subscribe()
		if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
			t.Fatalf("premise publish: %v", err)
		}
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("premise failed: the first notification never arrived")
		}
		b.Unsubscribe(ch)

		// Id 2 exists as far as Redis is concerned and never reached us.
		if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "2"); err != nil {
			t.Fatalf("set counter: %v", err)
		}

		_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1)
		if missed != nil {
			t.Fatalf("premise failed: the resume was served rather than reported as a gap; got %+v", missed)
		}
		if got := obs.snapshot(); got.resumeGaps != 1 {
			t.Fatalf("resumeGaps = %d, want 1 (%+v)", got.resumeGaps, got)
		}
	})

	t.Run("a resume below what this instance can vouch for", func(t *testing.T) {
		t.Parallel()
		// The LOCAL half: the shared counter agrees, so
		// resumeOutrunsLocalView says nothing and the gap comes from
		// this instance's own coverage window instead.
		b, mr := newMiniredisBus(t, 64)
		obs := newRecordingObserver()
		b.SetObserver(obs)

		ch, _ := b.Subscribe()
		if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
			t.Fatalf("premise publish: %v", err)
		}
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("premise failed: the first notification never arrived")
		}
		b.Unsubscribe(ch)

		// A HOLE moves knownFrom up past the cursor...
		b.fanOutLocally(Notification{ID: 9, Kind: KindComment, ItemRef: "TASK-9"}, b.currentGen())
		// ...and the shared counter is set to AGREE with what this
		// instance has seen, so resumeOutrunsLocalView finds no
		// disagreement and does NOT report. Without this the gap comes
		// from that path instead and the test proves nothing about this
		// one.
		if err := mr.Set(b.keys.Name(redisWatchSeqSuffix), "9"); err != nil {
			t.Fatalf("set counter: %v", err)
		}
		before := obs.snapshot().resumeGaps

		_, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1)
		if missed != nil {
			t.Fatalf("premise failed: the resume was served rather than reported as a gap; got %+v", missed)
		}
		if got := obs.snapshot().resumeGaps; got != before+1 {
			t.Fatalf("resumeGaps = %d, want %d — the local half of an unservable resume was not counted", got, before+1)
		}
	})

	t.Run("a resume this instance can serve", func(t *testing.T) {
		t.Parallel()
		b, _ := newMiniredisBus(t, 64)
		obs := newRecordingObserver()
		b.SetObserver(obs)

		ch, _ := b.Subscribe()
		if err := b.Publish(Notification{Kind: KindComment, ItemRef: "TASK-1"}); err != nil {
			t.Fatalf("premise publish: %v", err)
		}
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("premise failed: the first notification never arrived")
		}
		b.Unsubscribe(ch)

		if _, missed, _ := b.SubscribeAndReplaySince(context.Background(), 1); missed == nil {
			t.Fatal("premise failed: a current instance reported a gap")
		}
		if got := obs.snapshot(); got.resumeGaps != 0 {
			t.Fatalf("resumeGaps = %d on a servable resume, want 0 (%+v)", got.resumeGaps, got)
		}
	})
}

// reentrantObserver calls back INTO the bus from every callback. If the
// bus fired reports while holding its own mutex, any of these would
// deadlock it.
type reentrantObserver struct {
	b     *RedisBus
	calls chan string
}

func (o *reentrantObserver) reenter(name string) {
	// Two lock-taking entry points, so the test does not depend on which
	// one a future implementation happens to leave safe.
	//
	// Deliberately NOT SubscribeAndReplaySince: it can itself report a
	// resume gap, so calling it from an observer is unbounded mutual
	// recursion. Observer's contract names that hazard; the bus cannot
	// fix it on the caller's behalf.
	ch, _ := o.b.Subscribe()
	o.b.Unsubscribe(ch)
	select {
	case o.calls <- name:
	default:
	}
}

func (o *reentrantObserver) NotificationDropped(string) { o.reenter("dropped") }
func (o *reentrantObserver) SequenceGap(int64)          { o.reenter("gap") }
func (o *reentrantObserver) SequenceReset(string)       { o.reenter("reset") }
func (o *reentrantObserver) ResumeGap()                 { o.reenter("resume") }
func (o *reentrantObserver) ReceiveLoopExited()         { o.reenter("exit") }
func (o *reentrantObserver) HeartbeatPublishFailed()    { o.reenter("probe") }

// TestObserverMayReenterTheBus pins Observer's contract: reports fire
// with no bus lock held, so an observer that calls back into the bus
// cannot deadlock it.
//
// A deadlock hangs rather than failing, so the test bounds itself and
// reports the hang instead of letting the suite sit until go test's
// timeout.
func TestObserverMayReenterTheBus(t *testing.T) {
	t.Parallel()

	b, _ := newMiniredisBus(t, 64)
	obs := &reentrantObserver{b: b, calls: make(chan string, 16)}
	b.SetObserver(obs)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// A gap: ids 1 then 3.
		b.fanOutLocally(Notification{ID: 1, Kind: KindPush}, b.currentGen())
		b.fanOutLocally(Notification{ID: 3, Kind: KindPush}, b.currentGen())
		// An epoch change, through the other locked path.
		b.fanOutFromRedis("epoch-a", Notification{ID: 4, Kind: KindPush}, b.currentGen())
		b.fanOutFromRedis("epoch-b", Notification{ID: 1, Kind: KindPush}, b.currentGen())
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the bus deadlocked: an observer that calls back into it never returned")
	}

	// PREMISE: the observer actually ran. Without this the test would
	// pass against a bus that reported nothing at all, which does not
	// deadlock either.
	if len(obs.calls) == 0 {
		t.Fatal("premise failed: no observer callback fired, so re-entrancy was never exercised")
	}
}

// TestMemoryBusDropCounterIsMeaningfulWithoutRedis pins which counters
// are live on a single-process deployment: the slow-subscriber drop
// moves (MemoryBus has the same drop and the same observer), and
// everything sequence-related cannot. An operator builds alerts on
// exactly that distinction, so it is asserted rather than described.
func TestMemoryBusDropCounterIsMeaningfulWithoutRedis(t *testing.T) {
	t.Parallel()

	b := New()
	defer b.Close()
	obs := newRecordingObserver()
	b.SetObserver(obs)

	ch, _ := b.Subscribe()
	for i := 0; i < 65; i++ { // 64-deep buffer, then one more
		if err := b.Publish(Notification{Kind: KindPush, ItemRef: "TASK-1"}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	_ = ch // deliberately undrained

	got := obs.snapshot()
	if got.dropped[DropReasonSlowSubscriber] == 0 {
		t.Fatalf("the in-memory bus reported no drop, so the counter would be dead on a single-process deployment (%+v)", got)
	}
	// And nothing SEQUENCE-related fires here, which is the other half of
	// the claim: MemoryBus assigns contiguous ids and has no subscription
	// to lose.
	if got.gaps != 0 || got.resumeGaps != 0 || got.loopExits != 0 || len(got.resets) != 0 {
		t.Fatalf("the in-memory bus reported a sequence event it structurally cannot have (%+v)", got)
	}
}
