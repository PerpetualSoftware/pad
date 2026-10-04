package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/appfetch"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

// App webhook delivery from the outbox drain (SPEC-6 U10b, TASK-3408).

// appAdmitter adapts the store to the dispatcher's admission interface.
type appAdmitter struct{ st *store.Store }

func (a appAdmitter) AdmitAppDelivery(webhookID, event, collectionID, deliveryID string) (*webhooks.AppAdmission, error) {
	adm, err := a.st.AdmitAppDelivery(webhookID, event, collectionID, deliveryID)
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
	if !store.AppEventSubscribable[unit.eventType] {
		return false, nil
	}
	targets, err := s.store.ListAppWebhookTargets(unit.workspaceID, unit.eventType)
	if err != nil || len(targets) == 0 {
		return false, err
	}
	body, collectionID, err := store.BuildAppEventDTO(unit.eventType, unit.eventID, unit.occurredAt, unit.payload)
	if errors.Is(err, store.ErrNoAppProjection) {
		// Skipped and counted, never filled in from live state (§5).
		for range targets {
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
	for _, t := range targets {
		res := s.webhooks.DeliverAppEvent(adm, poster, webhooks.AppDelivery{
			WebhookID: t.WebhookID, Event: unit.eventType, CollectionID: collectionID, Body: body,
		})
		s.countAppDelivery(res.String())
		if res == webhooks.AppTransient || res == webhooks.AppDeferred {
			owed = true
		}
	}
	return owed, nil
}
