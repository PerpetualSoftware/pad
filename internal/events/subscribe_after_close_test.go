package events

import (
	"context"
	"testing"
	"time"
)

// closableBus is what this test needs of both implementations.
type closableBus interface {
	EventBus
	Close()
	SetObserver(Observer)
}

// BUG-2737. A subscribe after Close used to be admitted by MemoryBus: it was
// registered and handed a channel that nothing would ever close, so an SSE
// handler that subscribed late in shutdown ranged over it until srv.Shutdown's
// deadline. RedisBus already refused it with SubscribeFailed. Both buses must
// answer a late subscriber alike, so this runs every door on both.
//
// What each assertion is written against:
//   - outcome: the defect answers SubscribeOK.
//   - nil channel: the SSE handler defers Unsubscribe only on OK, so a
//     channel returned beside a refusal would be a registration nobody owes
//     an Unsubscribe for.
//   - SubscriberCount 0: the defect leaves the late subscriber registered.
//   - no ResumeGap on the resume door: a refused subscription is not a
//     resume this instance failed to serve (the admission-limit path's rule).
func TestSubscribeAfterCloseIsRefusedOnEveryDoor(t *testing.T) {
	buses := map[string]func(t *testing.T) closableBus{
		"memory": func(t *testing.T) closableBus {
			return New()
		},
		"redis": func(t *testing.T) closableBus {
			return newTestRedisBus(t)
		},
	}
	doors := map[string]func(ctx context.Context, b EventBus) (chan Event, SubscribeOutcome){
		"Subscribe": func(ctx context.Context, b EventBus) (chan Event, SubscribeOutcome) {
			ch, _, out := b.Subscribe(ctx, "ws1")
			return ch, out
		},
		"SubscribeIfAllowed": func(ctx context.Context, b EventBus) (chan Event, SubscribeOutcome) {
			ch, _, out := b.SubscribeIfAllowed(ctx, "ws1", 10)
			return ch, out
		},
		// A RESUMING call (sinceID > 0), so the resume-gap report is armed.
		"SubscribeAndReplaySince": func(ctx context.Context, b EventBus) (chan Event, SubscribeOutcome) {
			ch, _, _, out := b.SubscribeAndReplaySince(ctx, "ws1", 7, 10)
			return ch, out
		},
	}
	for busName, mk := range buses {
		for doorName, door := range doors {
			t.Run(busName+"/"+doorName, func(t *testing.T) {
				b := mk(t)
				obs := &recordingObserver{}
				b.SetObserver(obs)
				b.Close()

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				ch, out := door(ctx, b)

				if out != SubscribeFailed {
					t.Errorf("outcome = %s, want %s", out, SubscribeFailed)
				}
				if ch != nil {
					t.Errorf("a refused subscribe handed back a channel")
				}
				if n := b.SubscriberCount(); n != 0 {
					t.Errorf("SubscriberCount = %d after a refused subscribe, want 0", n)
				}
				obs.mu.Lock()
				gaps := len(obs.resumeGaps)
				obs.mu.Unlock()
				if gaps != 0 {
					t.Errorf("a refused subscribe reported %d resume gap(s), want 0", gaps)
				}
			})
		}
	}
}
