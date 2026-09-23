package server

import (
	"log/slog"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// publishActivityEvent is the ONE place a server producer hands an event to
// the activity bus, and the one place its Publish error is discarded
// (BUG-2732). Every producer publishes AFTER a write that already committed,
// and the event is a live notification of that durable state. Failing or
// annotating the HTTP response would report a successful write as a failure,
// so the error is logged and counted, never returned. That is the same ruling
// publishWatchNotification records for the watch bus (BUG-2699).
//
// THE LOG LINE MATTERS MORE HERE THAN IT DOES THERE. An event lost
// mid-subscription leaves no gap a connected client or a resume can detect
// (see events.EventBus.Publish), so this line and
// pad_eventbus_publish_failures_total are the only trace it leaves. It names
// the outcome, the event type and the workspace, and never the payload, which
// can carry item titles and actor names.
func (s *Server) publishActivityEvent(e events.Event) {
	if s.events == nil {
		return
	}
	if err := s.events.Publish(e); err != nil {
		s.publishFailures.record(err, e)
	}
}

// publishFailureLog rate-bounds publishActivityEvent's failure log.
//
// BOUNDED BECAUSE BOTH FAILURE MODES ARRIVE IN BURSTS: shutdown turns every
// in-flight publish into ErrBusClosed at once, and a Redis outage fails every
// publish until it ends. A bulk update alone publishes once per request, but a
// busy instance publishes per write. The counter carries the exact number, so
// the log's job is to say THAT it is happening and roughly how much. It emits a
// burst of publishFailureLogBurst lines, then one per publishFailureLogEvery,
// and each emitted line reports how many were suppressed since the last one.
//
// The zero value is ready to use, so a Server built as a literal in a test
// needs no constructor. now and log are test seams, nil in production.
type publishFailureLog struct {
	mu         sync.Mutex
	limiter    *rate.Limiter
	suppressed int64

	now func() time.Time
	log func(msg string, args ...any)
}

const (
	publishFailureLogBurst = 10
	publishFailureLogEvery = time.Second
)

func (l *publishFailureLog) record(err error, e events.Event) {
	l.mu.Lock()
	if l.limiter == nil {
		l.limiter = rate.NewLimiter(rate.Every(publishFailureLogEvery), publishFailureLogBurst)
	}
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	if !l.limiter.AllowN(now, 1) {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	suppressed := l.suppressed
	l.suppressed = 0
	logf := l.log
	l.mu.Unlock()

	if logf == nil {
		logf = slog.Warn
	}
	// Logged outside the lock: a slow log handler must not serialize every
	// failing publish behind it.
	logf("activity event not published; the write underneath committed, but connected clients are not told about this change and a reconnect will not replay it",
		"outcome", events.PublishFailureOutcome(err),
		"event_type", e.Type,
		"workspace", e.WorkspaceID,
		"error", err,
		"suppressed_since_last", suppressed)
}
