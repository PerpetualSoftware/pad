package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/PerpetualSoftware/pad/internal/appfetch"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

// App webhook delivery from the outbox drain (SPEC-6 U10b, TASK-3408).

// appAdmitter adapts the store to the dispatcher's admission interface.
type appAdmitter struct{ st *store.Store }

func (a appAdmitter) AdmitAppDelivery(webhookID, event, collectionID, occurredAt, deliveryID string) (*webhooks.AppAdmission, error) {
	adm, err := a.st.AdmitAppDelivery(webhookID, event, collectionID, occurredAt, deliveryID)
	var refused *store.AppDeliveryRefusedError
	if errors.As(err, &refused) {
		return nil, fmt.Errorf("%w: %s", webhooks.ErrAppDeliveryRefused, refused.Reason)
	}
	if err != nil {
		return nil, err
	}
	return &webhooks.AppAdmission{URL: adm.URL, Secret: adm.Secret, InstallID: adm.InstallID}, nil
}

func (a appAdmitter) EndAppDelivery(deliveryID string) error { return a.st.EndAppDelivery(deliveryID) }

// appPosterCache rebuilds the Poster only when the admin's private-origin
// list changes, so connections are reused between deliveries.
type appPosterCache struct {
	mu     sync.Mutex
	key    string
	poster *appfetch.Poster
}

func (s *Server) appPoster() (*appfetch.Poster, error) {
	private := s.appsPrivateOrigins()
	b, _ := json.Marshal(private)
	c := &s.appPosters
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.poster != nil && c.key == string(b) {
		return c.poster, nil
	}
	p, err := appfetch.NewPoster(private, 15*time.Second, s.appFetchTLS)
	if err != nil {
		return nil, err
	}
	c.key, c.poster = string(b), p
	return p, nil
}

func (s *Server) countAppDelivery(result string) {
	if s.metrics != nil {
		s.metrics.AppWebhookDeliveriesTotal.WithLabelValues(result).Inc()
	}
}

// deliverAppHooks delivers one outbox unit to the workspace's app hooks and
// reports whether the event is still owed to any of them. It reads the
// UNSTRIPPED payload: the app body is built from its app-projection block.
//
// An error is the server's (listing hooks, building the poster): nothing
// was attempted and the event is still owed.
func (s *Server) deliverAppHooks(unit outboxDelivery) (owed bool, err error) {
	// A member of a bulk operation is never delivered to an app, whether it
	// is claimed alone or folded under its header (item.bulk_updated, not
	// subscribable). Which of the two happens depends on claim timing, so
	// delivering the singles would hand an app an arbitrary subset of one
	// operation (codex r3 on U10b). DOC-3371 §5: a bulk human edit is
	// invisible to the app in v1; it resyncs through the API. Counted for
	// every app hook in the workspace, folded or single (codex r4).
	if unit.batchID != "" {
		hooks, err := s.store.ListAppWebhookTargets(unit.workspaceID, "")
		if err != nil {
			return false, err
		}
		for _, h := range hooks {
			// Recorded, so an owner hook's retry of this unit does not count
			// it again (codex r1 on U10c).
			st, err := s.store.DeliveryStatus(unit.eventID, h.WebhookID)
			if err != nil {
				return false, err
			}
			if store.DeliveryTerminal(st) {
				continue
			}
			if err := s.store.RecordDelivery(unit.eventID, h.WebhookID, store.DeliverySkipped, "bulk operation", false); err != nil {
				return false, err
			}
			s.countAppDelivery("skipped_bulk")
		}
		return false, nil
	}
	if !store.AppEventSubscribable[unit.eventType] {
		return false, nil
	}
	targets, err := s.store.ListAppWebhookTargets(unit.workspaceID, unit.eventType)
	if err != nil || len(targets) == 0 {
		return false, err
	}
	// Per-endpoint state (U10c): an endpoint whose row is terminal is done
	// with this event and never retried.
	var live []store.AppHookTarget
	for _, t := range targets {
		st, err := s.store.DeliveryStatus(unit.eventID, t.WebhookID)
		if err != nil {
			return false, err
		}
		if !store.DeliveryTerminal(st) {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		return false, nil
	}
	body, collectionID, err := store.BuildAppEventDTO(unit.eventType, unit.eventID, unit.occurredAt, unit.payload)
	if errors.Is(err, store.ErrNoAppProjection) {
		// Skipped and counted, never filled in from live state (§5), and
		// recorded so a retry for another endpoint does not count it again.
		for _, t := range live {
			if err := s.store.RecordDelivery(unit.eventID, t.WebhookID, store.DeliverySkipped, "no app projection", false); err != nil {
				return false, err
			}
			s.countAppDelivery("skipped_no_projection")
		}
		slog.Warn("app webhook: event has no app projection; skipped", "event_id", unit.eventID, "event", unit.eventType)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	poster, err := s.appPoster()
	if err != nil {
		return false, err
	}
	adm := appAdmitter{st: s.store}
	expired := appDeliveryExpired(unit.occurredAt, time.Now())
	for _, t := range live {
		// Only a hook subscribed to this event FOR THIS COLLECTION is a
		// target: anything else would be refused at admission, and must not
		// spend the install's rate budget or be dropped (codex r1 on U10c).
		if !store.AppHookSubscribes(t.Events, unit.eventType, collectionID) {
			continue
		}
		if expired {
			dropped, err := s.store.DropDelivery(unit.eventID, t.WebhookID, "undelivered after "+appWebhookDropAfter.String())
			if err != nil {
				return false, err
			}
			if dropped {
				s.countAppDelivery("dropped")
			}
			continue
		}
		installID := t.InstallID
		res := s.webhooks.DeliverAppEvent(adm, poster, webhooks.AppDelivery{
			WebhookID: t.WebhookID, Event: unit.eventType, CollectionID: collectionID, OccurredAt: unit.occurredAt, Body: body,
			// The cap is per ATTEMPT: a retry spends a token too.
			RateGate: func() bool { return s.appRate.allow(installID) },
		})
		s.countAppDelivery(res.String())
		status, attempted := appDeliveryStatus(res)
		if err := s.store.RecordDelivery(unit.eventID, t.WebhookID, status, "", attempted); err != nil {
			return false, err
		}
		if !store.DeliveryTerminal(status) {
			owed = true
		}
	}
	return owed, nil
}

// appWebhookDropAfter: an app delivery still undelivered this long after its
// event occurred is dropped and counted on the hook (dropped_count, shown to
// the owner) rather than retried. 24 h (lead ruling, day 86): long enough to
// ride out an app's ordinary outage or deploy, short enough that an app
// receiving a day-old change learns it should resync instead. Well inside
// the outbox's 7-day undispatched retention.
const appWebhookDropAfter = 24 * time.Hour

func appDeliveryExpired(occurredAt string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		return false
	}
	return now.Sub(t) > appWebhookDropAfter
}

// appDeliveryStatus maps an attempt's result to its recorded status, and
// whether it counts as an attempt (a refusal or a deferral sent nothing).
func appDeliveryStatus(r webhooks.AppResult) (string, bool) {
	switch r {
	case webhooks.AppDelivered:
		return store.DeliveryDelivered, true
	case webhooks.AppPermanent:
		return store.DeliveryPermanent, true
	case webhooks.AppRefused:
		return store.DeliveryRefused, false
	case webhooks.AppDeferred:
		return store.DeliveryDeferred, false
	case webhooks.AppRateLimited:
		// Deferred, not dropped and not charged as an attempt: the event
		// stays owed and a later pass tries again.
		return store.DeliveryRateLimited, false
	default:
		return store.DeliveryTransient, true
	}
}

// The per-install delivery rate cap (lead ruling, day 86): 600 a minute with
// a burst of 60. One busy workspace with an installed app can emit faster
// than any app needs to hear, and the drain delivers synchronously, so an
// uncapped app would take the drain's whole pass. Excess is DEFERRED (the
// event stays owed and goes on a later pass), never dropped. PER INSTANCE:
// the bucket lives in this process, so N instances allow up to N times the
// rate; acceptable for v1, where Pad runs single-instance (CLAUDE.md, collab).
const (
	appWebhookRatePerMinute = 600
	appWebhookRateBurst     = 60
)

type appRateLimiter struct {
	mu sync.Mutex
	by map[string]*rate.Limiter
	// limit and burst are the constants above; fields so a test can tighten
	// them.
	limit rate.Limit
	burst int
}

func (l *appRateLimiter) allow(installID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.by == nil {
		l.by = map[string]*rate.Limiter{}
		if l.limit == 0 {
			l.limit, l.burst = rate.Limit(appWebhookRatePerMinute)/60, appWebhookRateBurst
		}
	}
	lim, ok := l.by[installID]
	if !ok {
		if len(l.by) >= appRateLimiterPruneAt {
			l.pruneIdleLocked()
		}
		lim = rate.NewLimiter(l.limit, l.burst)
		l.by[installID] = lim
	}
	return lim.Allow()
}

// appRateLimiterPruneAt bounds the limiter map (codex r1 on U10c): past it,
// idle entries are dropped before a new one is added.
const appRateLimiterPruneAt = 1024

// pruneIdleLocked drops limiters whose bucket is full. A full bucket is
// exactly what a fresh limiter starts with, so dropping one changes no
// answer: an uninstalled or quiet install's entry goes, a busy one stays.
func (l *appRateLimiter) pruneIdleLocked() {
	now := time.Now()
	for id, lim := range l.by {
		if lim.TokensAt(now) >= float64(l.burst) {
			delete(l.by, id)
		}
	}
}
