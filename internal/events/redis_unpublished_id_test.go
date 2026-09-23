package events

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// BUG-2732: what a subscriber sees when an event ID is ASSIGNED but never
// PUBLISHED — the state a phase-1 publish leaves when its INCR succeeds and
// its PUBLISH fails. Checkpoint 1 on BUG-2732 proposed relying on that
// surfacing as a sequence gap, and the ruling there was to pin it before
// relying on it.
//
// IT DOES NOT SURFACE, and this test pins that as the current contract rather
// than a hope. Per-workspace IDs are non-consecutive by construction (the
// counter is global), so a skipped ID is indistinguishable from one that
// belonged to another workspace; see knownFrom in bus.go and BUG-2735. The
// consequence for BUG-2732 is that a failed publish is invisible to the
// subscriber AND to a resume from just below it, so counting the failure is
// the only trace it leaves.
//
// The failure is injected with a go-redis hook that refuses PUBLISH and
// nothing else, so the real phase-1 Publish runs its assignment, succeeds,
// and then fails exactly where production can.
func TestAnAssignedButUnpublishedIDIsNotSurfacedAsAGap(t *testing.T) {
	b, fail := newPublishFailingRedisBus(t)

	ch, gaps, outcome := b.Subscribe(context.Background(), "ws-1")
	if outcome != SubscribeOK {
		t.Fatalf("subscribe: %v", outcome)
	}
	defer b.Unsubscribe(ch)

	receive := func() Event {
		t.Helper()
		select {
		case e := <-ch:
			return e
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a published event")
			return Event{}
		}
	}

	if err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "a"}); err != nil {
		t.Fatalf("publish a: %v", err)
	}
	first := receive()

	fail.Store(true)
	err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "lost"})
	fail.Store(false)
	if err == nil || errors.Is(err, ErrBusClosed) {
		t.Fatalf("a PUBLISH failing after its assignment must report unconfirmed; got %v", err)
	}

	if err := b.Publish(Event{Type: ItemUpdated, WorkspaceID: "ws-1", ItemID: "b"}); err != nil {
		t.Fatalf("publish b: %v", err)
	}
	second := receive()

	// Precondition: an ID really was burned between the two delivered ones.
	// Without it the assertions below could pass on a bus that never skipped.
	if second.ID-first.ID != 2 {
		t.Fatalf("expected exactly one burned ID between %d and %d", first.ID, second.ID)
	}

	select {
	case <-gaps:
		t.Fatal("a gap was signalled for an assigned-but-unpublished ID; BUG-2732's plan assumed it is not, so revisit it")
	default:
	}

	got := b.EventsSince("ws-1", first.ID)
	if got == nil {
		t.Fatal("a resume from the first event was refused; BUG-2732's plan assumed it is served, so revisit it")
	}
	if len(got) != 1 || got[0].ID != second.ID {
		t.Fatalf("expected the resume to be served [%d] with the lost id silently absent, got %+v", second.ID, got)
	}
}

// failPublishHook refuses the PUBLISH command, and only it, while fail is set.
type failPublishHook struct{ fail *atomic.Bool }

func (h failPublishHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h failPublishHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h failPublishHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.fail.Load() && strings.EqualFold(cmd.Name(), "publish") {
			err := errors.New("injected: PUBLISH failed")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

func newPublishFailingRedisBus(t *testing.T) (*RedisBus, *atomic.Bool) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fail := &atomic.Bool{}
	client.AddHook(failPublishHook{fail: fail})
	t.Cleanup(func() { _ = client.Close() })
	b := NewRedisBus(client)
	t.Cleanup(b.Close)
	return b, fail
}
